package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloud-barista/cb-mcks/src/core/model"
	"github.com/cloud-barista/cb-mcks/src/core/model/tumblebug"
	"github.com/cloud-barista/cb-mcks/src/utils/config"
	"github.com/cloud-barista/cb-mcks/src/utils/lang"
	ssh "github.com/cloud-barista/cb-spider/cloud-control-manager/vm-ssh"
	"golang.org/x/sync/errgroup"

	logger "github.com/sirupsen/logrus"
)

func ListCluster(namespace string) (*model.ClusterList, error) {
	err := CheckNamespace(namespace)
	if err != nil {
		return nil, err
	}

	clusters := model.NewClusterList(namespace)
	err = clusters.SelectList()
	if err != nil {
		return nil, err
	}

	return clusters, nil
}

func GetCluster(namespace string, clusterName string) (*model.Cluster, error) {
	err := CheckNamespace(namespace)
	if err != nil {
		return nil, err
	}

	cluster := model.NewCluster(namespace, clusterName)
	err = cluster.Select()
	if err != nil {
		return nil, err
	}

	return cluster, nil
}

func CreateCluster(namespace string, req *model.ClusterReq) (*model.Cluster, error) {
	clusterName := req.Name
	cluster := model.NewCluster(namespace, clusterName)
	cluster.NetworkCni = req.Config.Kubernetes.NetworkCni
	mcisName := clusterName

	// Namespace 존재여부 확인
	err := CheckNamespace(namespace)
	if err != nil {
		return nil, err
	}

	// MCIS 존재여부 확인
	mcis := tumblebug.NewMCIS(namespace, mcisName)
	exists, err := mcis.GET()
	if err != nil {
		return cluster, err
	}
	if exists {
		return cluster, errors.New("MCIS already exists")
	}

	var nodeConfigInfos []NodeConfigInfo
	// control plane
	cp, err := SetNodeConfigInfos(req.ControlPlane, config.CONTROL_PLANE)
	if err != nil {
		return nil, err
	}
	nodeConfigInfos = append(nodeConfigInfos, cp...)

	// worker
	wk, err := SetNodeConfigInfos(req.Worker, config.WORKER)
	if err != nil {
		return nil, err
	}
	nodeConfigInfos = append(nodeConfigInfos, wk...)

	cIdx := 0
	wIdx := 0
	var nodes []model.Node
	var vmInfos []model.VMInfo

	for _, nodeConfigInfo := range nodeConfigInfos {
		// MCIR - 존재하면 재활용 없다면 생성 기준
		// 1. create vpc
		vpc, err := nodeConfigInfo.CreateVPC(namespace)
		if err != nil {
			return nil, err
		}

		// 2. create firewall
		fw, err := nodeConfigInfo.CreateFirewall(namespace)
		if err != nil {
			return nil, err
		}

		// 3. create sshKey
		sshKey, err := nodeConfigInfo.CreateSshKey(namespace)
		if err != nil {
			return nil, err
		}

		// 4. create image
		image, err := nodeConfigInfo.CreateImage(namespace)
		if err != nil {
			return nil, err
		}

		// 5. create spec
		spec, err := nodeConfigInfo.CreateSpec(namespace)
		if err != nil {
			return nil, err
		}

		// 6. vm
		for i := 0; i < nodeConfigInfo.Count; i++ {
			if nodeConfigInfo.Role == config.CONTROL_PLANE {
				cIdx++
			} else {
				wIdx++
			}

			vm := model.VM{
				Config:       nodeConfigInfo.Connection,
				VPC:          vpc.Name,
				Subnet:       vpc.Subnets[0].Name,
				Firewall:     []string{fw.Name},
				SSHKey:       sshKey.Name,
				Image:        image.Name,
				Spec:         spec.Name,
				UserAccount:  VM_USER_ACCOUNT,
				UserPassword: "",
				Description:  "",
			}

			vmInfo := model.VMInfo{
				Credential: sshKey.PrivateKey,
				Role:       nodeConfigInfo.Role,
				Csp:        nodeConfigInfo.Csp,
			}

			if nodeConfigInfo.Role == config.CONTROL_PLANE {
				vm.Name = lang.GetNodeName(config.CONTROL_PLANE, cIdx)
				if cIdx == 1 {
					vmInfo.IsCPLeader = true
					cluster.CpLeader = vm.Name
				}
			} else {
				vm.Name = lang.GetNodeName(config.WORKER, wIdx)
			}
			vmInfo.Name = vm.Name

			mcis.VMs = append(mcis.VMs, vm)
			vmInfos = append(vmInfos, vmInfo)
		}
	}

	// MCIS 생성
	mcis.Label = "mcks"
	logger.Infof("start create MCIS (name=%s)", mcisName)
	if err = mcis.POST(); err != nil {
		return nil, err
	}
	logger.Infof("create MCIS OK.. (name=%s)", mcisName)

	cpMcis := tumblebug.MCIS{}
	// 결과값 저장
	cluster.MCIS = mcisName
	for _, vm := range mcis.VMs {
		for _, vmInfo := range vmInfos {
			if vm.Name == vmInfo.Name {
				vm.Credential = vmInfo.Credential
				vm.Role = vmInfo.Role
				vm.Csp = vmInfo.Csp
				vm.IsCPLeader = vmInfo.IsCPLeader

				cpMcis.VMs = append(cpMcis.VMs, vm)
				break
			}
		}

		node := model.NewNodeVM(namespace, cluster.Name, vm)

		// insert node in store
		nodes = append(nodes, *node)
		err := node.Insert()
		if err != nil {
			return nil, err
		}
	}

	err = cluster.Insert()
	if err != nil {
		return nil, err
	}

	// bootstrap
	logger.Infoln("start k8s bootstrap")

	time.Sleep(2 * time.Second)

	err = cluster.Update()
	if err != nil {
		return nil, err
	}
	eg, _ := errgroup.WithContext(context.Background())

	for _, vm := range cpMcis.VMs {
		vm := vm
		eg.Go(func() error {
			if vm.Status != config.Running || vm.PublicIP == "" {
				return errors.New(fmt.Sprintf("Cannot do ssh, VM IP is not Running (name=%s, ip=%s, systemMessage=%s)", vm.Name, vm.PublicIP, vm.SystemMessage))
			}

			sshInfo := ssh.SSHInfo{
				UserName:   VM_USER_ACCOUNT,
				PrivateKey: []byte(vm.Credential),
				ServerPort: fmt.Sprintf("%s:22", vm.PublicIP),
			}
			err := vm.ConnectionTest(&sshInfo)
			if err != nil {
				return err
			}

			err = vm.CopyScripts(&sshInfo, cluster.NetworkCni)
			if err != nil {
				return err
			}

			err = vm.SetSystemd(&sshInfo, cluster.NetworkCni)
			if err != nil {
				return err
			}

			err = vm.Bootstrap(&sshInfo)
			if err != nil {
				return err
			}

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		cluster.Fail()
		deleteMcis(namespace, clusterName)
		return nil, err
	}

	// init & join
	var joinCmd []string
	IPs := GetControlPlaneIPs(cpMcis.VMs)

	logger.Infoln("start k8s init")
	for _, vm := range cpMcis.VMs {
		if vm.Role == config.CONTROL_PLANE && vm.IsCPLeader {
			sshInfo := ssh.SSHInfo{
				UserName:   VM_USER_ACCOUNT,
				PrivateKey: []byte(vm.Credential),
				ServerPort: fmt.Sprintf("%s:22", vm.PublicIP),
			}

			logger.Infof("install HAProxy (vm=%s)", vm.Name)
			err := vm.InstallHAProxy(&sshInfo, IPs)
			if err != nil {
				cluster.Fail()
				deleteMcis(namespace, clusterName)
				return nil, err
			}

			logger.Infoln("control plane init")
			var clusterConfig string
			joinCmd, clusterConfig, err = vm.ControlPlaneInit(&sshInfo, req.Config.Kubernetes)
			if err != nil {
				cluster.Fail()
				deleteMcis(namespace, clusterName)
				return nil, err
			}
			cluster.ClusterConfig = clusterConfig

			logger.Infoln("install networkCNI")
			err = vm.InstallNetworkCNI(&sshInfo, req.Config.Kubernetes.NetworkCni)
			if err != nil {
				cluster.Fail()
				deleteMcis(namespace, clusterName)
				return nil, err
			}
		}
	}
	logger.Infoln("end k8s init")

	logger.Infoln("start k8s join")
	for _, vm := range cpMcis.VMs {
		if vm.Role == config.CONTROL_PLANE && !vm.IsCPLeader {
			sshInfo := ssh.SSHInfo{
				UserName:   VM_USER_ACCOUNT,
				PrivateKey: []byte(vm.Credential),
				ServerPort: fmt.Sprintf("%s:22", vm.PublicIP),
			}
			logger.Infof("control plane join (vm=%s)", vm.Name)
			err := vm.ControlPlaneJoin(&sshInfo, &joinCmd[0])
			if err != nil {
				cluster.Fail()
				deleteMcis(namespace, clusterName)
				return nil, err
			}
		}
	}

	for _, vm := range cpMcis.VMs {
		if vm.Role == config.WORKER {
			sshInfo := ssh.SSHInfo{
				UserName:   VM_USER_ACCOUNT,
				PrivateKey: []byte(vm.Credential),
				ServerPort: fmt.Sprintf("%s:22", vm.PublicIP),
			}
			logger.Infof("worker join (vm=%s)", vm.Name)
			err := vm.WorkerJoin(&sshInfo, &joinCmd[1])
			if err != nil {
				cluster.Fail()
				deleteMcis(namespace, clusterName)
				return nil, err
			}
		}
	}
	logger.Infoln("end k8s join")

	logger.Infoln("start add node labels")
	for _, vm := range cpMcis.VMs {
		sshInfo := ssh.SSHInfo{
			UserName:   VM_USER_ACCOUNT,
			PrivateKey: []byte(vm.Credential),
			ServerPort: fmt.Sprintf("%s:22", vm.PublicIP),
		}
		err := vm.AddNodeLabels(&sshInfo)
		if err != nil {
			logger.Warnf("failed to add node labels (vm=%s, cause=%s)", vm.Name, err)
		}
	}
	logger.Infoln("end add node labels")

	cluster.Complete()
	cluster.Nodes = nodes

	return cluster, nil
}

func DeleteCluster(namespace string, clusterName string) (*model.Status, error) {
	err := CheckNamespace(namespace)
	if err != nil {
		return nil, err
	}

	mcisName := clusterName

	status := model.NewStatus()
	status.Code = model.STATUS_UNKNOWN

	logger.Infof("start delete Cluster (name=%s)", mcisName)
	mcis := tumblebug.NewMCIS(namespace, mcisName)
	cluster := model.NewCluster(namespace, clusterName)
	exist, err := mcis.GET()
	if err != nil {
		return status, err
	}
	if exist {
		logger.Infof("terminate MCIS (name=%s)", mcisName)
		if err = mcis.TERMINATE(); err != nil {
			logger.Errorf("terminate mcis error : %v", err)
			return status, err
		}
		time.Sleep(5 * time.Second)

		logger.Infof("delete MCIS (name=%s)", mcisName)
		if err = mcis.DELETE(); err != nil {
			if strings.Contains(err.Error(), "Deletion is not allowed") {
				logger.Infof("refine mcis (name=%s)", mcisName)
				if err = mcis.REFINE(); err != nil {
					logger.Errorf("refine MCIS error : %v", err)
					return status, err
				}
				logger.Infof("delete MCIS (name=%s)", mcisName)
				if err = mcis.DELETE(); err != nil {
					logger.Errorf("delete MCIS error : %v", err)
					return status, err
				}
			} else {
				logger.Errorf("delete MCIS error : %v", err)
				return status, err
			}
		}

		logger.Infof("delete MCIS OK.. (name=%s)", mcisName)
		status.Message = fmt.Sprintf("cluster %s has been deleted", mcisName)

		if err := cluster.Delete(); err != nil {
			status.Message = fmt.Sprintf("cluster %s has been deleted but cannot delete from the store", mcisName)
			return status, nil
		}
	} else {
		logger.Infof("delete Cluster skip (MCIS cannot find).. (name=%s)", mcisName)
		status.Message = fmt.Sprintf("cluster '%s' does not exist", mcisName)

		if err := cluster.Delete(); err != nil {
			status.Message = fmt.Sprintf("cluster %s does not exist and cannot delete from the store", mcisName)
			return status, nil
		}
	}

	status.Code = model.STATUS_SUCCESS
	return status, nil
}

func deleteMcis(namespace string, clusterName string) error {
	status, err := DeleteCluster(namespace, clusterName)
	if err != nil {
		logger.Errorf("failed to delete mcis (clusterName=%s, cause=%v)", clusterName, err)
		return nil
	}
	if status.Code == model.STATUS_SUCCESS {
		logger.Infof("mcis has been deleted (clusterName=%s)", clusterName)
		return nil
	}

	return nil
}
