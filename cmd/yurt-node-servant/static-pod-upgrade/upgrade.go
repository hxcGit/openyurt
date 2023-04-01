/*
Copyright 2023 The OpenYurt Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package upgrade

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"

	upgrade "github.com/openyurtio/openyurt/pkg/node-servant/static-pod-upgrade"
)

var (
	name      string
	namespace string
	manifest  string
	hash      string
	mode      string
)

// NewUpgradeCmd generates a new upgrade command
func NewUpgradeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "static-pod-upgrade",
		Short: "",
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Flags().VisitAll(func(flag *pflag.Flag) {
				klog.Infof("FLAG: --%s=%q", flag.Name, flag.Value)
			})

			if err := validate(); err != nil {
				klog.Fatalf("Fail to validate static pod upgrade args, %v", err)
			}

			ctrl, err := upgrade.New(name, namespace, manifest, hash, mode)
			if err != nil {
				klog.Fatalf("Fail to create static-pod-upgrade controller, %v", err)
			}

			if err = ctrl.Upgrade(); err != nil {
				klog.Fatalf("Fail to upgrade static pod, %v", err)
			}

			klog.Info("Static pod upgrade Success")
		},
		Args: cobra.NoArgs,
	}
	addFlags(cmd)

	return cmd
}

func addFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&name, "name", "", "The name of static pod which needs be upgraded")
	cmd.Flags().StringVar(&namespace, "namespace", "", "The namespace of static pod which needs be upgraded")
	cmd.Flags().StringVar(&manifest, "manifest", "", "The manifest file name of static pod which needs be upgraded")
	cmd.Flags().StringVar(&hash, "hash", "", "The hash value of new static pod specification")
	cmd.Flags().StringVar(&mode, "mode", "", "The upgrade mode which is used")
}

// Validate check if all the required arguments are valid
func validate() error {
	if name == "" || namespace == "" || manifest == "" || hash == "" || mode == "" {
		return fmt.Errorf("args can not be empty, name is %s, namespace is %s,manifest is %s, hash is %s,mode is %s",
			name, namespace, manifest, hash, mode)
	}

	// TODO: use constant value of static-pod controller
	if mode != "auto" && mode != "ota" {
		return fmt.Errorf("only support auto or ota upgrade mode")
	}

	return nil
}
