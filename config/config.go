package config

import "github.com/spf13/viper"

type AppConfig struct {
	EnableMonitoring bool   `mapstructure:"enable_monitoring"`
	Kubeconfig       string `mapstructure:"kubeconfig"`
	EnableBackup     bool   `mapstructure:"enable_backup"`
	InstallOLM       bool   `mapstructure:"install_olm"`
}

func ParseConfig() (*AppConfig, error) {
	viper.SetConfigType("yaml")
	c := &AppConfig{}
	err := viper.Unmarshal(c)
	return c, err
}
