package native

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/cnrancher/autok3s/pkg/cluster"
	"github.com/cnrancher/autok3s/pkg/common"
	"github.com/cnrancher/autok3s/pkg/providers"
	putil "github.com/cnrancher/autok3s/pkg/providers/utils"
	"github.com/cnrancher/autok3s/pkg/types"
	"github.com/cnrancher/autok3s/pkg/types/native"
	"github.com/cnrancher/autok3s/pkg/utils"

	"golang.org/x/sync/syncmap"
)

// providerName is the name of this provider.
const providerName = "native"

var (
	defaultUser       = "root"
	defaultSSHKeyPath = "~/.ssh/id_rsa"
)

type Native struct {
	*cluster.ProviderBase `json:",inline"`
	native.Options        `json:",inline"`
}

func init() {
	providers.RegisterProvider(providerName, func() (providers.Provider, error) {
		return newProvider(), nil
	})
}

func newProvider() *Native {
	base := cluster.NewBaseProvider()
	base.Provider = providerName
	return &Native{
		ProviderBase: base,
	}
}

func (p *Native) GetProviderName() string {
	return "native"
}

func (p *Native) GenerateClusterName() string {
	p.ContextName = p.Name
	return p.ContextName
}

func (p *Native) GenerateManifest() []string {
	// no need to support
	return nil
}

func (p *Native) GenerateMasterExtraArgs(cluster *types.Cluster, master types.Node) string {
	// no need to support.
	return ""
}

func (p *Native) GenerateWorkerExtraArgs(cluster *types.Cluster, worker types.Node) string {
	// no need to support.
	return ""
}

func (p *Native) CreateK3sCluster() (err error) {
	logFile, err := common.GetLogFile(p.Name)
	if err != nil {
		return err
	}
	c := &types.Cluster{
		Metadata: p.Metadata,
		Options:  p.Options,
		Status:   p.Status,
	}
	defer func() {
		if err != nil {
			p.Logger.Errorf("[%s] failed to create cluster: %v", p.GetProviderName(), err)
			if c == nil {
				c = &types.Cluster{
					Metadata: p.Metadata,
					Options:  p.Options,
					Status:   p.Status,
				}
			}
		}
		if err == nil && len(p.Status.MasterNodes) > 0 {
			p.Logger.Info(common.UsageInfoTitle)
			p.Logger.Infof(common.UsageContext, p.Name)
			p.Logger.Info(common.UsagePods)
		}
		os.Remove(filepath.Join(p.getStatePath(), fmt.Sprintf("%s_%s", c.Name, common.StatusCreating)))
		logFile.Close()
	}()

	p.Logger = common.NewLogger(common.Debug, logFile)
	p.Logger.Infof("[%s] executing create logic...", p.GetProviderName())
	err = p.saveState(c, common.StatusCreating)
	if err != nil {
		return err
	}

	// set ssh default value
	if p.SSHUser == "" {
		p.SSHUser = defaultUser
	}
	if p.SSHPassword == "" && p.SSHKeyPath == "" {
		p.SSHKeyPath = defaultSSHKeyPath
	}
	// assemble node status.
	if c, err = p.assembleNodeStatus(&p.SSH); err != nil {
		return err
	}
	c.SSH = p.SSH
	// initialize K3s cluster.
	if err = p.InitK3sCluster(c); err != nil {
		return
	}
	p.Logger.Infof("[%s] successfully executed create logic", p.GetProviderName())
	return nil
}

func (p *Native) JoinK3sNode() (err error) {
	if p.M == nil {
		p.M = new(syncmap.Map)
	}
	c := &types.Cluster{
		Metadata: p.Metadata,
		Options:  p.Options,
		Status:   p.Status,
	}
	logFile, err := common.GetLogFile(p.Name)
	if err != nil {
		return err
	}

	defer func() {
		os.Remove(filepath.Join(p.getStatePath(), fmt.Sprintf("%s_%s", c.Name, common.StatusUpgrading)))
		logFile.Close()
	}()

	p.Logger = common.NewLogger(common.Debug, logFile)
	p.Logger.Infof("[%s] executing join logic...", p.GetProviderName())
	// set ssh default value
	if p.SSHUser == "" {
		p.SSHUser = defaultUser
	}
	if p.SSHPassword == "" && p.SSHKeyPath == "" {
		p.SSHKeyPath = defaultSSHKeyPath
	}

	err = p.saveState(c, common.StatusUpgrading)
	if err != nil {
		return err
	}

	// assemble node status.
	if c, err = p.assembleNodeStatus(&p.SSH); err != nil {
		return err
	}

	added := &types.Cluster{
		Metadata: c.Metadata,
		Options:  c.Options,
		Status:   types.Status{},
		SSH:      p.SSH,
	}

	p.M.Range(func(key, value interface{}) bool {
		v := value.(types.Node)
		// filter the number of nodes that are not generated by current command.
		if v.Current {
			if v.Master {
				added.Status.MasterNodes = append(added.Status.MasterNodes, v)
			} else {
				added.Status.WorkerNodes = append(added.Status.WorkerNodes, v)
			}
			// for rollback
			p.M.Store(v.InstanceID, types.Node{Master: v.Master, RollBack: true, InstanceID: v.InstanceID, InstanceStatus: v.InstanceStatus, PublicIPAddress: v.PublicIPAddress, InternalIPAddress: v.InternalIPAddress, SSH: v.SSH})
		}
		return true
	})

	var (
		masterIps []string
		workerIps []string
	)

	for _, masterNode := range c.Status.MasterNodes {
		masterIps = append(masterIps, masterNode.PublicIPAddress...)
	}

	for _, workerNode := range c.Status.WorkerNodes {
		workerIps = append(workerIps, workerNode.PublicIPAddress...)
	}

	p.Options.MasterIps = strings.Join(masterIps, ",")
	p.Options.WorkerIps = strings.Join(workerIps, ",")
	// join K3s node.
	if err := p.Join(c, added); err != nil {
		return err
	}

	p.Logger.Infof("[%s] successfully executed join logic", p.GetProviderName())
	return nil
}

func (p *Native) Rollback() error {
	return p.RollbackCluster(func(ids []string) error {
		nodes := []types.Node{}
		for _, id := range ids {
			if node, ok := p.M.Load(id); ok {
				nodes = append(nodes, node.(types.Node))
			}
		}
		warnMsg := p.UninstallK3sNodes(nodes)
		for _, w := range warnMsg {
			p.Logger.Warnf("[%s] %s", p.GetProviderName(), w)
		}
		return nil
	})
}

func (p *Native) CreateCheck() error {
	if p.MasterIps == "" {
		return fmt.Errorf("[%s] cluster must have one master when create", p.GetProviderName())
	}
	// check file exists
	if p.SSHKeyPath != "" {
		sshPrivateKey := p.SSHKeyPath
		if strings.HasPrefix(sshPrivateKey, "~/") {
			usr, err := user.Current()
			if err != nil {
				return fmt.Errorf("[%s] failed to get user home directory: %v", p.GetProviderName(), err)
			}
			baseDir := usr.HomeDir
			sshPrivateKey = filepath.Join(baseDir, sshPrivateKey[2:])
		}
		if _, err := os.Stat(sshPrivateKey); err != nil {
			return err
		}
	}
	return nil
}

func (p *Native) JoinCheck() error {
	if p.MasterIps == "" && p.WorkerIps == "" {
		return fmt.Errorf("[%s] cluster must have one node when join", p.GetProviderName())
	}
	// check file exists
	if p.SSHKeyPath != "" {
		sshPrivateKey := p.SSHKeyPath
		if strings.HasPrefix(sshPrivateKey, "~/") {
			usr, err := user.Current()
			if err != nil {
				return fmt.Errorf("[%s] failed to get user home directory: %v", p.GetProviderName(), err)
			}
			baseDir := usr.HomeDir
			sshPrivateKey = filepath.Join(baseDir, sshPrivateKey[2:])
		}
		if _, err := os.Stat(sshPrivateKey); err != nil {
			return err
		}
	}
	return nil
}

func (p *Native) DeleteK3sCluster(f bool) error {
	return p.CommandNotSupport("delete")
}

func (p *Native) SSHK3sNode(ip string) error {
	return p.CommandNotSupport("ssh")
}

func (p *Native) CommandNotSupport(commandName string) error {
	return fmt.Errorf("[%s] dose not support command: [%s]", p.GetProviderName(), commandName)
}

func (p *Native) DescribeCluster(kubecfg string) *types.ClusterInfo {
	return &types.ClusterInfo{}
}

func (p *Native) GetCluster(kubecfg string) *types.ClusterInfo {
	return &types.ClusterInfo{}
}

func (p *Native) IsClusterExist() (bool, []string, error) {
	return false, []string{}, nil
}

func (p *Native) SetConfig(config []byte) error {
	c, err := p.SetClusterConfig(config)
	if err != nil {
		return err
	}
	sourceOption := reflect.ValueOf(&p.Options).Elem()
	b, err := json.Marshal(c.Options)
	if err != nil {
		return err
	}
	opt := &native.Options{}
	err = json.Unmarshal(b, opt)
	if err != nil {
		return err
	}
	targetOption := reflect.ValueOf(opt).Elem()
	utils.MergeConfig(sourceOption, targetOption)
	return nil
}

func (p *Native) SetOptions(opt []byte) error {
	sourceOption := reflect.ValueOf(&p.Options).Elem()
	option := &native.Options{}
	err := json.Unmarshal(opt, option)
	if err != nil {
		return err
	}
	targetOption := reflect.ValueOf(option).Elem()
	utils.MergeConfig(sourceOption, targetOption)
	return nil
}

func (p *Native) GetProviderOptions(opt []byte) (interface{}, error) {
	options := &native.Options{}
	err := json.Unmarshal(opt, options)
	return options, err
}

func (p *Native) assembleNodeStatus(ssh *types.SSH) (*types.Cluster, error) {
	if p.MasterIps != "" {
		masterIps := strings.Split(p.MasterIps, ",")
		p.syncNodesMap(masterIps, true, ssh)
	}

	if p.WorkerIps != "" {
		workerIps := strings.Split(p.WorkerIps, ",")
		p.syncNodesMap(workerIps, false, ssh)
	}

	p.M.Range(func(key, value interface{}) bool {
		v := value.(types.Node)
		nodes := p.Status.WorkerNodes
		if v.Master {
			nodes = p.Status.MasterNodes
		}
		index, b := putil.IsExistedNodes(nodes, v.InstanceID)
		if !b {
			nodes = append(nodes, v)
		} else {
			nodes[index].Current = false
			nodes[index].RollBack = false
		}

		if v.Master {
			p.Status.MasterNodes = nodes
		} else {
			p.Status.WorkerNodes = nodes
		}
		return true
	})

	p.Master = strconv.Itoa(len(p.MasterNodes))
	p.Worker = strconv.Itoa(len(p.WorkerNodes))

	return &types.Cluster{
		Metadata: p.Metadata,
		Options:  p.Options,
		Status:   p.Status,
	}, nil
}

func (p *Native) syncNodesMap(ipList []string, master bool, ssh *types.SSH) {
	for _, ip := range ipList {
		currentID := strings.Replace(ip, ".", "-", -1)
		p.M.Store(currentID, types.Node{
			Master:            master,
			RollBack:          true,
			InstanceID:        currentID,
			InstanceStatus:    "-",
			InternalIPAddress: []string{ip},
			PublicIPAddress:   []string{ip},
			Current:           true,
			SSH:               *ssh,
		})
	}
}

func (p *Native) getStatePath() string {
	return filepath.Join(common.GetLogPath(), p.GetProviderName())
}

func (p *Native) saveState(c *types.Cluster, status string) error {
	statePath := p.getStatePath()
	err := utils.EnsureFolderExist(statePath)
	if err != nil {
		return err
	}
	return utils.WriteYaml(c, statePath, fmt.Sprintf("%s_%s", c.Name, status))
}
