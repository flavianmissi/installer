package agent

import (
	"context"
	"net"
	"net/url"

	"github.com/go-openapi/strfmt"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/openshift/assisted-service/api/v1beta1"
	"github.com/openshift/assisted-service/client"
	"github.com/openshift/assisted-service/client/events"
	"github.com/openshift/assisted-service/client/installer"
	"github.com/openshift/assisted-service/models"
	"github.com/openshift/installer/pkg/asset/agent/agentconfig"
	"github.com/openshift/installer/pkg/asset/agent/image"
	"github.com/openshift/installer/pkg/asset/agent/manifests"
	"github.com/openshift/installer/pkg/asset/installconfig"
	assetstore "github.com/openshift/installer/pkg/asset/store"
	"github.com/openshift/installer/pkg/types/agent"
)

// NodeZeroRestClient is a struct to interact with the Agent Rest API that is on node zero.
type NodeZeroRestClient struct {
	Client     *client.AssistedInstall
	ctx        context.Context
	config     client.Config
	NodeZeroIP string
	NodeSSHKey []string
}

// NewNodeZeroRestClient Initialize a new rest client to interact with the Agent Rest API on node zero.
func NewNodeZeroRestClient(ctx context.Context, assetDir string) (*NodeZeroRestClient, error) {
	restClient := &NodeZeroRestClient{}
	agentConfigAsset := &agentconfig.AgentConfig{}
	agentManifestsAsset := &manifests.AgentManifests{}
	installConfigAsset := &installconfig.InstallConfig{}

	assetStore, err := assetstore.NewStore(assetDir)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create asset store")
	}

	agentConfig, agentConfigError := assetStore.Load(agentConfigAsset)
	agentManifests, manifestError := assetStore.Load(agentManifestsAsset)
	installConfig, installConfigError := assetStore.Load(installConfigAsset)

	if agentConfigError != nil {
		logrus.Debug(errors.Wrapf(agentConfigError, "failed to load %s", agentConfigAsset.Name()))
	}
	if manifestError != nil {
		logrus.Debug(errors.Wrapf(manifestError, "failed to load %s", agentManifestsAsset.Name()))
	}
	if installConfigError != nil {
		logrus.Debug(errors.Wrapf(installConfigError, "failed to load %s", installConfigAsset.Name()))
	}
	if agentConfigError != nil || manifestError != nil || installConfigError != nil {
		return nil, errors.New("failed to load AgentConfig, NMStateConfig, or InstallConfig")
	}

	var RendezvousIP string
	var rendezvousIPError error
	var emptyNMStateConfigs []*v1beta1.NMStateConfig

	if agentConfig != nil && agentManifests != nil {
		RendezvousIP, rendezvousIPError = image.RetrieveRendezvousIP(agentConfig.(*agentconfig.AgentConfig).Config, agentManifests.(*manifests.AgentManifests).NMStateConfigs)
	} else if agentConfig == nil && agentManifests != nil {
		RendezvousIP, rendezvousIPError = image.RetrieveRendezvousIP(&agent.Config{}, agentManifests.(*manifests.AgentManifests).NMStateConfigs)
	} else if agentConfig != nil && agentManifests == nil {
		RendezvousIP, rendezvousIPError = image.RetrieveRendezvousIP(agentConfig.(*agentconfig.AgentConfig).Config, emptyNMStateConfigs)
	} else {
		return nil, errors.New("both AgentConfig and NMStateConfig are empty")
	}
	if rendezvousIPError != nil {
		return nil, rendezvousIPError
	}

	// Get SSH Keys which can be used to determine if Rest API failures are due to network connectivity issues
	if installConfig != nil {
		restClient.NodeSSHKey = append(restClient.NodeSSHKey, installConfig.(*installconfig.InstallConfig).Config.SSHKey)
	}

	config := client.Config{}
	config.URL = &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(RendezvousIP, "8090"),
		Path:   client.DefaultBasePath,
	}
	client := client.New(config)

	restClient.Client = client
	restClient.ctx = ctx
	restClient.config = config
	restClient.NodeZeroIP = RendezvousIP

	return restClient, nil
}

// IsRestAPILive Determine if the Agent Rest API on node zero has initialized
func (rest *NodeZeroRestClient) IsRestAPILive() bool {
	// GET /v2/infraenvs
	listInfraEnvsParams := installer.NewListInfraEnvsParams()
	_, err := rest.Client.Installer.ListInfraEnvs(rest.ctx, listInfraEnvsParams)
	if err != nil {
		return false
	}
	return true
}

// GetRestAPIServiceBaseURL Return the url of the Agent Rest API on node zero
func (rest *NodeZeroRestClient) GetRestAPIServiceBaseURL() *url.URL {
	return rest.config.URL
}

// GetInfraEnvEvents Return the event list for the provided infraEnvID from the Agent Rest API
func (rest *NodeZeroRestClient) GetInfraEnvEvents(infraEnvID *strfmt.UUID) (models.EventList, error) {
	listEventsParams := &events.V2ListEventsParams{InfraEnvID: infraEnvID}
	clusterEventsResult, err := rest.Client.Events.V2ListEvents(rest.ctx, listEventsParams)
	if err != nil {
		return nil, err
	}
	return clusterEventsResult.Payload, nil
}

// getClusterID Return the cluster ID assigned by the Agent Rest API
func (rest *NodeZeroRestClient) getClusterID() (*strfmt.UUID, error) {
	// GET /v2/clusters and return first result
	listClusterParams := installer.NewV2ListClustersParams()
	clusterResult, err := rest.Client.Installer.V2ListClusters(rest.ctx, listClusterParams)
	if err != nil {
		return nil, err
	}
	clusterList := clusterResult.Payload
	if len(clusterList) == 1 {
		clusterID := clusterList[0].ID
		return clusterID, nil
	} else if len(clusterList) == 0 {
		logrus.Debug("cluster is not registered in rest API")
		return nil, nil
	} else {
		logrus.Infof("found too many clusters. number of clusters found: %d", len(clusterList))
		return nil, nil
	}
}

// getClusterID Return the infraEnv ID associated with the cluster in the Agent Rest API
func (rest *NodeZeroRestClient) getClusterInfraEnvID() (*strfmt.UUID, error) {
	// GET /v2/infraenvs and return first result
	listInfraEnvParams := installer.NewListInfraEnvsParams()
	infraEnvResult, err := rest.Client.Installer.ListInfraEnvs(rest.ctx, listInfraEnvParams)
	if err != nil {
		return nil, err
	}
	infraEnvList := infraEnvResult.Payload
	if len(infraEnvList) == 1 {
		clusterInfraEnvID := infraEnvList[0].ID
		return clusterInfraEnvID, nil
	} else if len(infraEnvList) == 0 {
		logrus.Debug("infraenv is not registered in rest API")
		return nil, nil
	} else {
		logrus.Infof("found too many infraenvs. number of infraenvs found: %d", len(infraEnvList))
		return nil, nil
	}
}
