/*
Copyright © 2023 NAME HERE <EMAIL ADDRESS>
*/
package cmd

import (
	"fmt"
	"os"

	"github.com/gen1us2k/everest-provisioner/config"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "everest-provisioner",
	Short: "A brief description of your application",
	Long: `A longer description that spans multiple lines and likely contains
examples and usage of using your application. For example:

Cobra is a CLI library for Go that empowers applications.
This application is a tool to generate the needed files
to quickly create a Cobra application.`,
	// Uncomment the following line if your bare application
	// has an action associated with it:
	// Run: func(cmd *cobra.Command, args []string) { },
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
	c, err := config.ParseConfig()
	if err != nil {
		os.Exit(1)
	}
	fmt.Println(c)
}

func init() {
	// Here you will define your flags and configuration settings.
	// Cobra supports persistent flags, which, if defined here,
	// will be global for your application.

	// rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.everest-provisioner.yaml)")

	// Cobra also supports local flags, which will only run
	// when this action is called directly.
	rootCmd.Flags().BoolP("enable_monitoring", "m", true, "Enable monitoring")
	viper.BindPFlag("enable_monitoring", rootCmd.Flags().Lookup("enable_monitoring"))
	rootCmd.Flags().BoolP("enable_backup", "b", false, "Enable backups")
	viper.BindPFlag("enable_backup", rootCmd.Flags().Lookup("enable_backup"))
	rootCmd.Flags().BoolP("install_olm", "o", true, "Install OLM")
	viper.BindPFlag("install_olm", rootCmd.Flags().Lookup("install_olm"))
	rootCmd.Flags().StringP("kubeconfig", "k", "~/.kube/config", "specify kubeconfig")
	viper.BindPFlag("kubeconfig", rootCmd.Flags().Lookup("kubeconfig"))
}
