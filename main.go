package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	errors2 "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	//"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"

	"gopkg.in/yaml.v2"
	//"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"
	//cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fatih/color"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/instrumenta/kubeval/kubeval"
	"github.com/instrumenta/kubeval/log"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var (
	version                 = "dev"
	commit                  = "none"
	date                    = "unknown"
	directories             = []string{}
	ignoredPathPatterns = []string{}

	// forceColor tells kubeval to use colored output even if
	// stdout is not a TTY
	forceColor bool

	config = kubeval.NewDefaultConfig()
)

// RootCmd represents the the command to run when kubeval is run
var RootCmd = &cobra.Command{
	Short:   "Validate everything in a K8s cluster against the relevant schema",
	Long:    `Validate everything in a K8s cluster against the relevant schema`,
	Version: fmt.Sprintf("Version: %s\nCommit: %s\nDate: %s\n", version, commit, date),
	Run: func(cmd *cobra.Command, args []string) {
		if config.IgnoreMissingSchemas && !config.Quiet {
			log.Warn("Set to ignore missing schemas")
		}

		// This is not particularly secure but we highlight that with the name of
		// the config item. It would be good to also support a configurable set of
		// trusted certificate authorities as in the `--certificate-authority`
		// kubectl option.
		if config.InsecureSkipTLSVerify {
			http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{
				InsecureSkipVerify: true,
			}
		}

		success := true
		//windowsStdinIssue := false
		outputManager := kubeval.GetOutputManager(config.OutputFormat)

		//stat, err := os.Stdin.Stat()
		//if err != nil {
		//	// Stat() will return an error on Windows in both Powershell and
		//	// console until go1.9 when nothing is passed on stdin.
		//	// See https://github.com/golang/go/issues/14853.
		//	if runtime.GOOS != "windows" {
		//		log.Error(err)
		//		os.Exit(1)
		//	} else {
		//		windowsStdinIssue = true
		//	}
		//}
		// Assert that colors will definitely be used if requested
		if forceColor {
			color.NoColor = false
		}
		// We detect whether we have anything on stdin to process if we have no arguments
		// or if the argument is a -
		//notty := (stat.Mode() & os.ModeCharDevice) == 0
		//noFileOrDirArgs := (len(args) < 1 || args[0] == "-") && len(directories) < 1


		//build the k8s client from kubeconfig
		//@todo see if the kubeconfig flag works
		var kubeconfig *string
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
		} else {
			kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
		}
		//kubeConfigFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag()
		//matchVersionKubeConfigFlags := cmdutil.NewMatchVersionFlags(kubeConfigFlags)
		//f := cmdutil.NewFactory(matchVersionKubeConfigFlags)
		cfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			log.Error(err)
			os.Exit(1)
		}
		clientset, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			log.Error(err)
			os.Exit(1)
		}

		//grab all the api-group-resources from the cluster
		apiGroupResources, err := restmapper.GetAPIGroupResources(clientset.DiscoveryClient)
		if err != nil {
			log.Error(err)
			os.Exit(1)
		}

		dyn, err := dynamic.NewForConfig(cfg)
		if err != nil {
			log.Error(err)
			os.Exit(1)
		}



		schemaCache := kubeval.NewSchemaCache()
		var aggResults []kubeval.ValidationResult
		mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(clientset.DiscoveryClient))
		//iterate through each group/version/resource
		for _, apiGroupResource := range apiGroupResources {
			//log.Success(fmt.Sprintf("API - %v", apiGroupResource))
			for _, version := range apiGroupResource.Group.Versions {
				resources := apiGroupResource.VersionedResources[version.Version]
				//log.Success(fmt.Sprintf("Version - %v", version.Version))
				for _, resource := range resources {
					//subresources not of interest
					if strings.Contains(resource.Name, "/") {
						continue
					}
					//log.Success(fmt.Sprintf("Got resource: %v", resource))

					//grab all the instances of this resource from the cluster
					gvk := schema.GroupVersionKind{Group: apiGroupResource.Group.Name, Version: version.Version, Kind: resource.Kind}
					mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
					if err != nil {
						log.Error(err)
						os.Exit(1)
					}

					dr := dyn.Resource(mapping.Resource)
					res, err := dr.List(context.Background(), metav1.ListOptions{})
					if errors2.IsNotFound(err) {
						//log.Warn(fmt.Sprintf("Nothing found for: %v, %v, %v", mapping.Resource.Group, mapping.Resource.Version, mapping.Resource.Resource))
						continue
					} else if _, isStatus := err.(*errors2.StatusError); isStatus {
						//log.Error(fmt.Errorf("Error getting resource: %v - %v %v %v", statusError, mapping.Resource.Group, mapping.Resource.Version, mapping.Resource.Resource))
						continue
					}

					//log.Success(fmt.Sprintf("**** Resource/Instances: %v/%v/%v, %d", mapping.Resource.Group, mapping.Resource.Version, mapping.Resource.Resource, strconv.Itoa(len(res.Items))))
					for _, i := range res.Items {
						//log.Success(fmt.Sprintf("  - GOT ME A: %v/%v/%v, %v/%v", i.GroupVersionKind().Group, i.GroupVersionKind().Version, i.GroupVersionKind().Kind, i.GetNamespace(), i.GetName()))
					 	objYaml, err := yaml.Marshal(i.Object)
					 	if err != nil{
							log.Warn(fmt.Sprintf("  - Unable to serialize to YAML: %v/%v/%v, %v/%v", i.GroupVersionKind().Group, i.GroupVersionKind().Version, i.GroupVersionKind().Kind, i.GetNamespace(), i.GetName()))
						}
						results, err := kubeval.ValidateWithCache(objYaml, schemaCache, config)
						r := results[0]
						err = outputManager.Put(r)
						if !r.ValidatedAgainstSchema {
							//log.Warn("...Skippinng additional of same kind")
							break
						}
						if err != nil {
							log.Error(err)
							os.Exit(1)
						}

						aggResults = append(aggResults, results...)
						success = success && !hasErrors(aggResults)
					}
				}
			}
		}
		// flush any final logs which may be sitting in the buffer
		err = outputManager.Flush()
		if err != nil {
			log.Error(err)
			os.Exit(1)
		}

		if !success {
			os.Exit(1)
		}



		//if err != nil {
		//	log.Error(err)
		//	os.Exit(1)
		//}
		//log.Success(fmt.Sprintf("%v", groups))
		//log.Success(fmt.Sprintf("%v", resources))



//		if noFileOrDirArgs && !windowsStdinIssue && notty {
//			buffer := new(bytes.Buffer)
//			_, err := io.Copy(buffer, os.Stdin)
//			if err != nil {
//				log.Error(err)
//				os.Exit(1)
//			}
//			schemaCache := kubeval.NewSchemaCache()
//			config.FileName = viper.GetString("filename")
//			results, err := kubeval.ValidateWithCache(buffer.Bytes(), schemaCache, config)
//			if err != nil {
//				log.Error(err)
//				os.Exit(1)
//			}
//			success = !hasErrors(results)
//
//			for _, r := range results {
//				err = outputManager.Put(r)
//				if err != nil {
//					log.Error(err)
//					os.Exit(1)
//				}
//			}
//		} else {
//			if len(args) < 1 && len(directories) < 1 {
//				log.Error(errors.New("You must pass at least one file as an argument, or at least one directory to the directories flag"))
//				os.Exit(1)
//			}
//			schemaCache := kubeval.NewSchemaCache()
//			files, err := aggregateFiles(args)
//			if err != nil {
//				log.Error(err)
//				success = false
//			}
//
//			var aggResults []kubeval.ValidationResult
//			for _, fileName := range files {
//				filePath, _ := filepath.Abs(fileName)
//				fileContents, err := ioutil.ReadFile(filePath)
//				if err != nil {
//					log.Error(fmt.Errorf("Could not open file %v", fileName))
//					earlyExit()
//					success = false
//					continue
//				}
//				config.FileName = fileName
//				results, err := kubeval.ValidateWithCache(fileContents, schemaCache, config)
//				if err != nil {
//					log.Error(err)
//					earlyExit()
//					success = false
//					continue
//				}
//
//				for _, r := range results {
//					err := outputManager.Put(r)
//					if err != nil {
//						log.Error(err)
//						os.Exit(1)
//					}
//				}
//
//				aggResults = append(aggResults, results...)
//			}
//
//			// only use result of hasErrors check if `success` is currently truthy
//			success = success && !hasErrors(aggResults)
//		}
//
//		// flush any final logs which may be sitting in the buffer
//		err = outputManager.Flush()
//		if err != nil {
//			log.Error(err)
//			os.Exit(1)
//		}
//
//		if !success {
//			os.Exit(1)
//		}
	},
}

// hasErrors returns truthy if any of the provided results
// contain errors.
func hasErrors(res []kubeval.ValidationResult) bool {
	for _, r := range res {
		if len(r.Errors) > 0 {
			return true
		}
	}
	return false
}

// isIgnored returns whether the specified filename should be ignored.
func isIgnored(path string) (bool, error) {
	for _, p := range ignoredPathPatterns {
		m, err := regexp.MatchString(p, path)
		if err != nil {
			return false, err
		}
		if m {
			return true, nil
		}
	}
	return false, nil
}

func aggregateFiles(args []string) ([]string, error) {
	files := make([]string, len(args))
	copy(files, args)

	var allErrors *multierror.Error
	for _, directory := range directories {
		err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			ignored, err := isIgnored(path)
			if err != nil {
				return err
			}
			if !info.IsDir() && (strings.HasSuffix(info.Name(), ".yaml") || strings.HasSuffix(info.Name(), ".yml")) && !ignored {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			allErrors = multierror.Append(allErrors, err)
		}
	}

	return files, allErrors.ErrorOrNil()
}

func earlyExit() {
	if config.ExitOnError {
		os.Exit(1)
	}
}

// Execute adds all child commands to the root command sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := RootCmd.Execute(); err != nil {
		log.Error(err)
		os.Exit(-1)
	}
}

func init() {
	rootCmdName := filepath.Base(os.Args[0])
	if strings.HasPrefix(rootCmdName, "kubectl-") {
		rootCmdName = strings.Replace(rootCmdName, "-", " ", 1)
	}
	RootCmd.Use = fmt.Sprintf("%s <file> [file...]", rootCmdName)
	kubeval.AddKubevalFlags(RootCmd, config)
	RootCmd.Flags().BoolVarP(&forceColor, "force-color", "", false, "Force colored output even if stdout is not a TTY")
	RootCmd.SetVersionTemplate(`{{.Version}}`)
	RootCmd.Flags().StringSliceVarP(&directories, "directories", "d", []string{}, "A comma-separated list of directories to recursively search for YAML documents")
	RootCmd.Flags().StringSliceVarP(&ignoredPathPatterns, "ignored-path-patterns", "i", []string{}, "A comma-separated list of regular expressions specifying paths to ignore")
	RootCmd.Flags().StringSliceVarP(&ignoredPathPatterns, "ignored-filename-patterns", "", []string{}, "An alias for ignored-path-patterns")
	
	viper.SetEnvPrefix("KUBEVAL")
	viper.AutomaticEnv()
	viper.BindPFlag("schema_location", RootCmd.Flags().Lookup("schema-location"))
	viper.BindPFlag("filename", RootCmd.Flags().Lookup("filename"))
}

func main() {
	Execute()
}
