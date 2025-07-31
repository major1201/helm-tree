/*
Copyright © 2019 NAME HERE <EMAIL ADDRESS>

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
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/fatih/color"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"helm.sh/helm/v3/pkg/action"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	_ "k8s.io/client-go/plugin/pkg/client/auth" // combined authprovider import
	"k8s.io/client-go/rest"
	"k8s.io/klog"
)

const (
	allNamespacesFlag = "all-namespaces"
	colorFlag         = "color"
)

var cf *genericclioptions.ConfigFlags

// This variable is populated by goreleaser
var version string

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:          "tree RELEASE",
	SilenceUsage: true, // for when RunE returns an error
	Short:        "Show sub-resources of a helm release",
	Example: "  helm tree my-release\n" +
		"  helm tree -n kube-public my-release",
	Args:    cobra.ExactArgs(1),
	RunE:    run,
	Version: versionString(),
}

// versionString returns the version prefixed by 'v'
// or an empty string if no version has been populated by goreleaser.
// In this case, the --version flag will not be added by cobra.
func versionString() string {
	if len(version) == 0 {
		return ""
	}
	return "v" + version
}

func atoi(a string, dv int) int {
	i, err := strconv.ParseInt(a, 10, 32)
	if err != nil {
		return dv
	}
	return int(i)
}

func atof32(a string, dv float32) float32 {
	f64, err := strconv.ParseFloat(a, 32)
	if err != nil {
		return dv
	}
	return float32(f64)
}

func SetPtrAsEnvvarIfNil(ptr *string, env string) {
	if ptr == nil || *ptr == "" {
		*ptr = os.Getenv(env)
	}
}

func run(command *cobra.Command, args []string) error {
	allNs, err := command.Flags().GetBool(allNamespacesFlag)
	if err != nil {
		allNs = false
	}

	colorArg, err := command.Flags().GetString(colorFlag)
	if err != nil {
		return err
	}
	if colorArg == "always" {
		color.NoColor = false
	} else if colorArg == "never" {
		color.NoColor = true
	} else if colorArg != "auto" {
		return errors.Errorf("invalid value for --%s", colorFlag)
	}

	// parse helm envvars to ConfigFlags
	SetPtrAsEnvvarIfNil(cf.APIServer, "HELM_KUBEAPISERVER")
	SetPtrAsEnvvarIfNil(cf.Impersonate, "HELM_KUBEASUSER")
	SetPtrAsEnvvarIfNil(cf.CAFile, "HELM_KUBECAFILE")
	SetPtrAsEnvvarIfNil(cf.Context, "HELM_KUBECONTEXT")
	*cf.Insecure = os.Getenv("HELM_KUBEINSECURE_SKIP_TLS_VERIFY") == "true"
	SetPtrAsEnvvarIfNil(cf.TLSServerName, "HELM_KUBETLS_SERVER_NAME")
	SetPtrAsEnvvarIfNil(cf.BearerToken, "HELM_KUBETOKEN")
	SetPtrAsEnvvarIfNil(cf.KubeConfig, "KUBECONFIG")
	SetPtrAsEnvvarIfNil(cf.Namespace, "HELM_NAMESPACE")

	restConfig, err := cf.ToRESTConfig()
	if err != nil {
		return err
	}
	restConfig.WarningHandler = rest.NoWarnings{}
	restConfig.QPS = atof32(os.Getenv("HELM_QPS"), 1000)
	restConfig.Burst = atoi(os.Getenv("HELM_BURST_LIMIT"), 1000)
	if restConfig.QPS < 1 {
		restConfig.QPS = min(100, float32(restConfig.Burst))
	}

	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to construct dynamic client: %w", err)
	}
	dc, err := cf.ToDiscoveryClient()
	if err != nil {
		return err
	}

	apis, err := findAPIs(dc)
	if err != nil {
		return err
	}
	klog.V(3).Info("completed querying APIs list")

	releaseName, err := figureOutName(args)
	if err != nil {
		return err
	}
	klog.V(3).Infof("parsed releaseName=%v", releaseName)

	ns := getNamespace()
	klog.V(2).Infof("namespace=%s allNamespaces=%v", ns, allNs)

	// init helm client
	actionConfig := new(action.Configuration)
	if err = actionConfig.Init(cf, ns, "", klog.Infof); err != nil {
		return err
	}
	cmdGet := action.NewGet(actionConfig)
	release, err := cmdGet.Run(releaseName)
	if err != nil {
		return err
	}
	var manifests []unstructured.Unstructured
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(release.Manifest), 4096)
	for {
		var obj unstructured.Unstructured
		derr := decoder.Decode(&obj)
		if derr == io.EOF {
			break
		}
		if derr != nil {
			continue
		}
		if obj.Object == nil {
			continue
		}
		manifests = append(manifests, obj)
	}

	klog.V(2).Infof("querying all api objects")
	apiObjects, err := getAllResources(dyn, apis.resources(), allNs)
	if err != nil {
		return fmt.Errorf("error while querying api objects: %w", err)
	}
	klog.V(2).Infof("found total %d api objects", len(apiObjects))

	objDir := newObjectDirectory(apiObjects)
	var objs []*unstructured.Unstructured
	for _, obj := range manifests {
		if obj.GetNamespace() == "" {
			kind := obj.GetKind()
			apiResults := apis.lookup(kind)
			if len(apiResults) == 0 {
				return fmt.Errorf("could not find api kind %q", kind)
			} else if len(apiResults) > 1 {
				names := make([]string, 0, len(apiResults))
				for _, a := range apiResults {
					names = append(names, fullAPIName(a))
				}
				return fmt.Errorf("ambiguous kind %q. use one of these as the KIND disambiguate: [%s]", kind,
					strings.Join(names, ", "))
			}
			api := apiResults[0]

			if api.r.Namespaced && obj.GetNamespace() == "" {
				obj.SetNamespace(ns)
			}
		}

		real := setRealObject(apiObjects, obj)
		if real != nil {
			objs = append(objs, real)
		} else {
			objs = append(objs, &obj)
		}
	}
	treeView(os.Stderr, objDir, objs)
	klog.V(2).Infof("done printing tree view")
	return nil
}

func setRealObject(apiObjects []unstructured.Unstructured, obj unstructured.Unstructured) *unstructured.Unstructured {
	for _, real := range apiObjects {
		if real.GetKind() == obj.GetKind() && real.GetNamespace() == obj.GetNamespace() && real.GetName() == obj.GetName() {
			real := real
			return &real
		}
	}
	return nil
}

func init() {
	klog.InitFlags(nil)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	// hide all glog flags except for -v
	flag.CommandLine.VisitAll(func(f *flag.Flag) {
		if f.Name != "v" {
			pflag.Lookup(f.Name).Hidden = true
		}
	})

	cf = genericclioptions.NewConfigFlags(true)

	rootCmd.Flags().BoolP(allNamespacesFlag, "A", false, "query all objects in all API groups, both namespaced and non-namespaced")
	rootCmd.Flags().StringP(colorFlag, "c", "auto", "Enable or disable color output. This can be 'always', 'never', or 'auto' (default = use color only if using tty). The flag is overridden by the NO_COLOR env variable if set.")

	// cf.AddFlags(rootCmd.Flags())
	if err := flag.Set("logtostderr", "true"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set logtostderr flag: %v\n", err)
		os.Exit(1)
	}
}

func main() {
	defer klog.Flush()
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
