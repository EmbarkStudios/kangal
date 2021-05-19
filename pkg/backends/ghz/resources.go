package ghz

import (
	"fmt"

	"github.com/hellofresh/kangal/pkg/backends"
	loadTestV1 "github.com/hellofresh/kangal/pkg/kubernetes/apis/loadtest/v1"
	"go.uber.org/zap"
	batchV1 "k8s.io/api/batch/v1"
	coreV1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	loadTestJobName           = "loadtest-job"
	loadTestFileConfigMapName = "loadtest-testfile"

	configFileName = "config.json"
)

// NewTestFileConfigMap creates a new configmap containing ghz config file
func (b *Backend) NewTestFileConfigMap(loadTest loadTestV1.LoadTest) *coreV1.ConfigMap {
	testfile := loadTest.Spec.TestFile

	return &coreV1.ConfigMap{
		ObjectMeta: metaV1.ObjectMeta{
			Name: loadTestFileConfigMapName,
		},
		Data: map[string]string{
			configFileName: testfile,
		},
	}
}

// NewJob creates a new job that runs ghz
func (b *Backend) NewJob(
	loadTest loadTestV1.LoadTest,
	loadTestFileConfigMap *coreV1.ConfigMap,
	reportURL string,
) *batchV1.Job {
	logger := b.logger.With(
		zap.String("loadtest", loadTest.GetName()),
		zap.String("namespace", loadTest.Status.Namespace),
	)

	ownerRef := metaV1.NewControllerRef(&loadTest, loadTestV1.SchemeGroupVersion.WithKind("LoadTest"))

	imageRef := fmt.Sprintf("%s:%s", loadTest.Spec.MasterConfig.Image, loadTest.Spec.MasterConfig.Tag)
	if imageRef == ":" {
		imageRef = fmt.Sprintf("%s:%s", b.image.Image, b.image.Tag)
		logger.Warn("Loadtest.Spec.MasterConfig is empty; using default image", zap.String("imageRef", imageRef))
	}

	envVars := []coreV1.EnvVar{}
	if "" != reportURL {
		envVars = append(envVars, coreV1.EnvVar{
			Name:  "REPORT_PRESIGNED_URL",
			Value: reportURL,
		})
	}

	return &batchV1.Job{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      loadTestJobName,
			Namespace: loadTest.Status.Namespace,
			Labels: map[string]string{
				"name": loadTestJobName,
			},
			OwnerReferences: []metaV1.OwnerReference{*ownerRef},
		},
		Spec: batchV1.JobSpec{
			BackoffLimit: nil,
			Template: coreV1.PodTemplateSpec{
				ObjectMeta: metaV1.ObjectMeta{
					Labels: map[string]string{
						"name": loadTestJobName,
					},
					Annotations: b.podAnnotations,
				},
				Spec: coreV1.PodSpec{
					RestartPolicy: "Never",
					Volumes: []coreV1.Volume{
						{
							Name: "testfile",
							VolumeSource: coreV1.VolumeSource{
								ConfigMap: &coreV1.ConfigMapVolumeSource{
									LocalObjectReference: coreV1.LocalObjectReference{
										Name: loadTestFileConfigMap.GetName(),
									},
								},
							},
						},
					},
					Containers: []coreV1.Container{
						{
							Name:      "ghz",
							Image:     imageRef,
							Env:       envVars,
							Resources: backends.BuildResourceRequirements(b.resources),
							Args: []string{
								"--config=/data/config.json",
								"--output=/results",
								"--format=html",
							},
							VolumeMounts: []coreV1.VolumeMount{
								{
									Name:      "testfile",
									MountPath: "/data/config.json",
									SubPath:   "config.json",
								},
							},
						},
					},
				},
			},
		},
	}
}

// determineLoadTestStatusFromJobs reads existing job statuses and determines what the loadtest status should be
func determineLoadTestStatusFromJobs(job *batchV1.Job) loadTestV1.LoadTestPhase {
	if job.Status.Failed > int32(0) {
		return loadTestV1.LoadTestErrored
	}

	if job.Status.Active > int32(0) {
		return loadTestV1.LoadTestRunning
	}

	if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
		return loadTestV1.LoadTestStarting
	}

	return loadTestV1.LoadTestFinished
}
