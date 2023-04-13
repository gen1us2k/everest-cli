package cli

import (
	"github.com/gen1us2k/everest-provisioner/config"
	"github.com/gen1us2k/everest-provisioner/kubernetes"
)

type CLI struct {
	config     *config.AppConfig
	kubeClient *kubernetes.Kubernetes
}

func New(c *config.AppConfig) (*CLI, error) {
	cli := &CLI{config: c}
	k, err := kubernetes.New(c.Kubeconfig)
	if err != nil {
		return nil, err
	}
	cli.kubeClient = k
	return cli, nil
}
