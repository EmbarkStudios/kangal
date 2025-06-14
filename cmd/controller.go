package cmd

import (
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.18.0"

	"github.com/kelseyhightower/envconfig"
	"github.com/spf13/cobra"
	kubeInformers "k8s.io/client-go/informers"
	kubernetesClient "k8s.io/client-go/kubernetes"

	"github.com/hellofresh/kangal/pkg/controller"
	"github.com/hellofresh/kangal/pkg/core/observability"
	"github.com/hellofresh/kangal/pkg/kubernetes"
	clientSet "github.com/hellofresh/kangal/pkg/kubernetes/generated/clientset/versioned"
	informers "github.com/hellofresh/kangal/pkg/kubernetes/generated/informers/externalversions"
)

// reconcileDistribution defines the bucket boundaries for the histogram of reconcile latency metric
// Bucket boundaries are 10ms, 100ms, 1s, 10s, 30s and 60s.
var reconcileDistribution = []float64{10, 100, 1000, 10000, 30000, 60000}

type controllerCmdOptions struct {
	kubeConfig           string
	masterURL            string
	namespaceLabels      []string
	namespaceAnnotations []string
	podAnnotations       []string
	nodeSelectors        []string
	tolerations          []string
}

// NewControllerCmd creates a new controller command
func NewControllerCmd() *cobra.Command {
	opts := &controllerCmdOptions{}

	cmd := &cobra.Command{
		Use:     "controller",
		Short:   "Run controller to communicate to k8s infrastructure",
		Aliases: []string{"c"},
		RunE: func(cmd *cobra.Command, args []string) error {
			var cfg controller.Config
			if err := envconfig.Process("", &cfg); err != nil {
				return fmt.Errorf("could not load config from env: %w", err)
			}

			// set some command line option to controller config
			cfg, err := populateCfgFromOpts(cfg, opts)
			if err != nil {
				return err
			}

			logger, _, err := observability.NewLogger(cfg.Logger)
			if err != nil {
				return fmt.Errorf("could not build logger instance: %w", err)
			}

			pe, err := prometheus.New()
			if err != nil {
				return fmt.Errorf("could not build prometheus exporter: %w", err)
			}

			kubeCfg, err := kubernetes.BuildClientConfig(cfg.MasterURL, cfg.KubeConfig, cfg.KubeClientTimeout)
			if err != nil {
				return fmt.Errorf("error building kubeConfig: %w", err)
			}

			kubeClient, err := kubernetesClient.NewForConfig(kubeCfg)
			if err != nil {
				return fmt.Errorf("error building kubernetes clientSet: %w", err)
			}

			kangalClient, err := clientSet.NewForConfig(kubeCfg)
			if err != nil {
				return fmt.Errorf("error building kangal clientSet: %w", err)
			}

			provider := metric.NewMeterProvider(
				metric.WithReader(pe),
				metric.WithResource(
					resource.NewSchemaless(semconv.ServiceNameKey.String("kangal-controller"))),
				metric.WithView(metric.NewView(
					metric.Instrument{Name: "kangal_reconcile_latency"},
					metric.Stream{Aggregation: metric.AggregationExplicitBucketHistogram{
						Boundaries: reconcileDistribution,
					}},
				)))
			statsReporter, err := controller.NewMetricsReporter(provider.Meter("controller"))
			if err != nil {
				return fmt.Errorf("error getting stats client:  %w", err)
			}

			kubeInformerFactory := kubeInformers.NewSharedInformerFactory(kubeClient, time.Second*30)
			kangalInformerFactory := informers.NewSharedInformerFactory(kangalClient, time.Second*30)

			return controller.Run(cfg, controller.Runner{
				Logger:         logger,
				Exporter:       pe,
				KubeClient:     kubeClient,
				KangalClient:   kangalClient,
				StatsReporter:  statsReporter,
				KubeInformer:   kubeInformerFactory,
				KangalInformer: kangalInformerFactory,
			})
		},
	}

	flags := cmd.PersistentFlags()
	flags.StringVar(&opts.kubeConfig, "kubeconfig", "", "(optional) Absolute path to the kubeConfig file. Only required if out-of-cluster.")
	flags.StringVar(&opts.masterURL, "master-url", "", "The address of the Kubernetes API server. Overrides any value in kubeConfig. Only required if out-of-cluster.")
	flags.StringSliceVar(&opts.namespaceLabels, "namespace-label", []string{}, "label will be attached to the loadtest namespace")
	flags.StringSliceVar(&opts.namespaceAnnotations, "namespace-annotation", []string{}, "annotation will be attached to the loadtest namespace")
	flags.StringSliceVar(&opts.podAnnotations, "pod-annotation", []string{}, "annotation will be attached to the loadtest pods")
	flags.StringSliceVar(&opts.nodeSelectors, "node-selector", []string{}, "nodeSelector rules will be attached to the loadtest pods")
	flags.StringSliceVar(&opts.tolerations, "tolerations", []string{}, "toleration rules to be applied to the loadtest pods")

	return cmd
}

func populateCfgFromOpts(cfg controller.Config, opts *controllerCmdOptions) (controller.Config, error) {
	var err error

	cfg.MasterURL = opts.masterURL
	cfg.KubeConfig = opts.kubeConfig

	cfg.NamespaceLabels, err = convertKeyPairStringToMap(opts.namespaceLabels)
	if err != nil {
		return controller.Config{}, fmt.Errorf("failed to convert namepsace labels: %w", err)
	}

	cfg.NamespaceAnnotations, err = convertKeyPairStringToMap(opts.namespaceAnnotations)
	if err != nil {
		return controller.Config{}, fmt.Errorf("failed to convert namepsace annotations: %w", err)
	}
	cfg.PodAnnotations, err = convertKeyPairStringToMap(opts.podAnnotations)
	if err != nil {
		return controller.Config{}, fmt.Errorf("failed to convert pod annotations: %w", err)
	}
	cfg.NodeSelectors, err = convertKeyPairStringToMap(opts.nodeSelectors)
	if err != nil {
		return controller.Config{}, fmt.Errorf("failed to convert node selectors: %w", err)
	}
	cfg.Tolerations, err = kubernetes.ParseTolerations(opts.tolerations)
	if err != nil {
		return controller.Config{}, fmt.Errorf("failed to convert node selectors: %w", err)
	}
	return cfg, nil
}

func convertKeyPairStringToMap(s []string) (map[string]string, error) {
	m := make(map[string]string, len(s))
	for _, a := range s {
		// We need to split annotation string to key value map and remove special chars from it:
		// Before string: iam.amazonaws.com/role: "arn:aws:iam::id:role/some-role"
		// After map[string]string: iam.amazonaws.com/role -> arn:aws:iam::id:role/some-role
		a = strings.Replace(a, `"`, ``, -1)
		str := strings.SplitN(a, ":", 2)
		if len(str) < 2 {
			return nil, fmt.Errorf(fmt.Sprintf("Annotation %q is invalid", a))
		}
		key, value := str[0], str[1]
		m[key] = value
	}
	return m, nil
}
