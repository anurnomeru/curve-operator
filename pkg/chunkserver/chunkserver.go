package chunkserver

import (
	"context"
	"time"

	"github.com/coreos/pkg/capnslog"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/types"

	curvev1 "github.com/opencurve/curve-operator/api/v1"
	"github.com/opencurve/curve-operator/pkg/clusterd"
	"github.com/opencurve/curve-operator/pkg/k8sutil"
)

const (
	AppName             = "curve-chunkserver"
	ConfigMapNamePrefix = "curve-chunkserver-conf"

	// ContainerPath is the mount path of data and log
	Prefix                      = "/curvebs/chunkserver"
	ChunkserverContainerDataDir = "/curvebs/chunkserver/data"
	ChunkserverContainerLogDir  = "/curvebs/chunkserver/logs"

	// start.sh
	startChunkserverConfigMapName     = "start-chunkserver-conf"
	startChunkserverScriptFileDataKey = "start_chunkserver.sh"
	startChunkserverMountPath         = "/curvebs/tools/sbin/start_chunkserver.sh"
)

type Cluster struct {
	context         clusterd.Context
	namespacedName  types.NamespacedName
	spec            curvev1.CurveClusterSpec
	dataDirHostPath string
	logDirHostPath  string
	confDirHostPath string
	ownerInfo       *k8sutil.OwnerInfo
}

var logger = capnslog.NewPackageLogger("github.com/opencurve/curve-operator", "chunkserver")

func New(context clusterd.Context,
	namespacedName types.NamespacedName,
	spec curvev1.CurveClusterSpec,
	ownerInfo *k8sutil.OwnerInfo,
	dataDirHostPath string,
	logDirHostPath string,
	confDirHostPath string) *Cluster {
	return &Cluster{
		context:         context,
		namespacedName:  namespacedName,
		spec:            spec,
		dataDirHostPath: dataDirHostPath,
		logDirHostPath:  logDirHostPath,
		confDirHostPath: confDirHostPath,
		ownerInfo:       ownerInfo,
	}
}

// Start begins the chunkserver daemon
func (c *Cluster) Start(nodeNameIP map[string]string) error {
	logger.Infof("start running chunkserver in namespace %q", c.namespacedName.Namespace)

	if !c.spec.Storage.UseSelectedNodes && (len(c.spec.Storage.Nodes) == 0 || len(c.spec.Storage.Devices) == 0) {
		return errors.New("useSelectedNodes is set to false but no node specified")
	}

	if c.spec.Storage.UseSelectedNodes && len(c.spec.Storage.SelectedNodes) == 0 {
		return errors.New("useSelectedNodes is set to false but selectedNodes not be specified")
	}

	logger.Info("starting to prepare the chunk file")

	// 1. startProvisioningOverNodes format device and prepare chunk files
	err := c.startProvisioningOverNodes(nodeNameIP)
	if err != nil {
		return errors.Wrap(err, "failed to provision chunkfilepool")
	}

	// 2. wait all job finish to complete format and wait MDS election success.
	k8sutil.UpdateCondition(context.TODO(), &c.context, c.namespacedName, curvev1.ConditionTypeFormatedReady, curvev1.ConditionTrue, curvev1.ConditionFormatingChunkfilePoolReason, "Formating chunkfilepool")
	oneMinuteTicker := time.NewTicker(20 * time.Second)
	defer oneMinuteTicker.Stop()

	chn := make(chan bool, 1)
	ctx, canf := context.WithTimeout(context.Background(), time.Duration(24*60*60*time.Second))
	defer canf()
	go c.checkJobStatus(ctx, oneMinuteTicker, chn)

	// block here unitl timeout(24 hours) or all jobs has been successed.
	flag := <-chn
	if !flag {
		// TODO: delete all jobs that has created.
		return errors.New("Format job is not completed in 24 hours and exit with -1")
	}
	k8sutil.UpdateCondition(context.TODO(), &c.context, c.namespacedName, curvev1.ConditionTypeFormatedReady, curvev1.ConditionTrue, curvev1.ConditionFormatChunkfilePoolReason, "Formating chunkfilepool successed")

	logger.Info("all jobs run completed in 24 hours")

	// 2. create physical pool
	_, err = c.runCreatePoolJob(nodeNameIP, "physical_pool")
	if err != nil {
		return errors.Wrap(err, "failed to create physical pool")
	}
	logger.Info("create physical pool successed")

	// 3. startChunkServers start all chunkservers for each device of every node
	// 4. wait all chunkservers online before create logical pool
	err = c.startChunkServers()
	if err != nil {
		return errors.Wrap(err, "failed to start chunkserver")
	}

	// 5. create logical pool
	_, err = c.runCreatePoolJob(nodeNameIP, "logical_pool")
	if err != nil {
		return errors.Wrap(err, "failed to create physical pool")
	}
	logger.Info("create logical pool successed")

	k8sutil.UpdateCondition(context.TODO(), &c.context, c.namespacedName, curvev1.ConditionTypeChunkServerReady, curvev1.ConditionTrue, curvev1.ConditionChunkServerClusterCreatedReason, "Chunkserver cluster has been created")

	return nil
}
