package cli

import (
	"context"
	"os"

	"github.com/gen1us2k/everest-provisioner/config"
	"github.com/gen1us2k/everest-provisioner/kubernetes"
	"github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/sirupsen/logrus"
)

type CLI struct {
	config     *config.AppConfig
	kubeClient *kubernetes.Kubernetes
	l          *logrus.Entry
}

const (
	namespace              = "default"
	catalogSourceNamespace = "olm"
	operatorGroup          = "percona-operators-group"
	catalogSource          = "percona-dbaas-catalog"
)

func New(c *config.AppConfig) (*CLI, error) {
	cli := &CLI{config: c}
	k, err := kubernetes.New(c.Kubeconfig)
	if err != nil {
		return nil, err
	}
	cli.kubeClient = k
	cli.l = logrus.WithField("component", "cli")
	return cli, nil
}

func (c *CLI) ProvisionCluster() error {
	c.l.Info("started provisioning the cluster")
	ctx := context.TODO()
	if c.config.InstallOLM {
		c.l.Info("Installing Operator Lifecycle Manager")
		if err := c.kubeClient.InstallOLMOperator(ctx); err != nil {
			c.l.Error("failed installing OLM")
			return err
		}
	}
	c.l.Info("OLM has been installed")
	c.l.Info("installing Victoria Metrics operator")
	channel, ok := os.LookupEnv("DBAAS_VM_OP_CHANNEL")
	if !ok || channel == "" {
		channel = "stable-v0"
	}
	params := kubernetes.InstallOperatorRequest{
		Namespace:              namespace,
		Name:                   "victoriametrics-operator",
		OperatorGroup:          operatorGroup,
		CatalogSource:          catalogSource,
		CatalogSourceNamespace: catalogSourceNamespace,
		Channel:                channel,
		InstallPlanApproval:    v1alpha1.ApprovalManual,
	}

	if err := c.kubeClient.InstallOperator(ctx, params); err != nil {
		c.l.Error("failed installing victoria metrics operator")
		return err
	}
	c.l.Info("Victoria metrics operator has been installed")
	c.l.Info("Installing PXC operator")
	channel, ok = os.LookupEnv("DBAAS_PXC_OP_CHANNEL")
	if !ok || channel == "" {
		channel = "stable-v1"
	}

	if err := c.kubeClient.InstallOperator(ctx, params); err != nil {
		c.l.Error("failed installing PXC operator")
		return err
	}
	c.l.Info("PXC operator has been installed")
	c.l.Info("Installing PSMDB operator")
	channel, ok = os.LookupEnv("DBAAS_PSMDB_OP_CHANNEL")
	if !ok || channel == "" {
		channel = "stable-v1"
	}
	params.Name = "percona-server-mongodb-operator"
	params.Channel = channel
	if err := c.kubeClient.InstallOperator(ctx, params); err != nil {
		c.l.Error("failed installing PSMDB operator")
		return err
	}
	c.l.Info("PSMDB operator has been installed")
	c.l.Info("Installing DBaaS operator")
	channel, ok = os.LookupEnv("DBAAS_DBAAS_OP_CHANNEL")
	if !ok || channel == "" {
		channel = "stable-v0"
	}
	params.Name = "dbaas-operator"
	params.Channel = channel
	if err := c.kubeClient.InstallOperator(ctx, params); err != nil {
		c.l.Error("failed installing DBaaS operator")
		return err
	}
	c.l.Info("DBaaS operator has been installed")
	if c.config.EnableMonitoring {
		c.l.Info("Started setting up monitoring")
		if err := c.provisionPMMMonitoring(); err != nil {
			return err
		}
	}
	return nil
}
func (c *CLI) provisionPMMMonitoring() error {
	return nil
}
