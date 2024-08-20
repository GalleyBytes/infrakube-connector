package tfhandler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/galleybytes/monitor/projects/terraform-operator-remote-controller/pkg/util"
	tfv1beta1 "github.com/galleybytes/terraform-operator/pkg/apis/tf/v1beta1"
	"github.com/isaaguilar/kedge"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type StatusCheckResponse struct {
	DidStart     bool   `json:"did_start"`
	DidComplete  bool   `json:"did_complete"`
	CurrentState string `json:"current_state"`
	CurrentTask  string `json:"current_task"`
}

func (i informer) backgroundQueueWorker() {
	go i.worker()
}

func (i informer) worker() {
	ctx := context.TODO()
	log.Println("Queue worker started")
MainLoop:
	for {
		if i.queue.Len() == 0 {
			// log.Println("No queue events found")
			time.Sleep(3 * time.Second)
			continue
		}

		// TODO Keep track of how many attempts queued items have done. Remove after a certain threshold.

		tf := i.queue.PopFront()
		if !shouldPoll(tf) {
			continue
		}
		name := tf.Name
		namespace := tf.Namespace

		if i.clusterName == "" || name == "" || namespace == "" {
			// The resulting request will be malformed unless all vars are defined
			// TODO Determine what causes undefined fields
			continue
		}

		failureRequeueRate := 100 * time.Second

		result, err := i.clientset.Cluster(i.clusterName).Status(namespace, name).Read(ctx, &tf)
		if err != nil {
			i.requeueAfter(tf, failureRequeueRate, fmt.Sprintf("ERROR: %s", err))
			continue
		}

		if result == nil {
			i.requeueAfter(tf, failureRequeueRate, "API did not return results")
			continue
		}

		if result.Data.StatusInfo.Message != "" {
			i.requeueAfter(tf, 30*time.Second, result.Data.StatusInfo.Message)
			continue
		}

		if !result.IsSuccess {
			// Even after a failure, continue to check for a success. There is a possibility a debug session
			// may fix the session.
			i.requeueAfter(tf, failureRequeueRate, result.ErrMsg)
			continue
		}

		statusCheckResponse, ok := result.Data.Data.([]any)
		if !ok {
			i.requeueAfter(tf, failureRequeueRate, fmt.Sprintf("ERROR status check response in unexpected format %T", result.Data.Data))
			continue
		}

		for _, responseItem := range statusCheckResponse {
			if _, ok := responseItem.(map[string]any); !ok {
				i.requeueAfter(tf, failureRequeueRate, fmt.Sprintf("ERROR status check response item in unexpected format %T", responseItem))
				continue MainLoop
			}
			if _, ok := responseItem.(map[string]any)["did_complete"].(bool); !ok {
				i.requeueAfter(tf, failureRequeueRate, fmt.Sprintf("ERROR unexpected json format for 'did_complete' key %T", responseItem))
				continue MainLoop
			}
			if !responseItem.(map[string]any)["did_complete"].(bool) {
				i.requeueAfter(tf, 30*time.Second, "Waiting for workflow completion")
				continue MainLoop
			}
		}

		poll, err := i.clientset.Cluster(i.clusterName).Poll(namespace, name).Read(ctx, &tf)
		if err != nil {
			i.requeueAfter(tf, failureRequeueRate, fmt.Sprintf("ERROR: %s", err))
			continue
		}

		if poll == nil {
			i.requeueAfter(tf, failureRequeueRate, "API did not return results")
			continue
		}

		if strings.Contains(poll.Data.StatusInfo.Message, "workflow has not completed") {
			i.requeueAfter(tf, 30*time.Second, "Waiting for workflow completion")
			continue
		}

		if !poll.IsSuccess {
			// Even after a failure, continue to check for a success. There is a possibility a debug session
			// may fix the session.
			i.requeueAfter(tf, failureRequeueRate, poll.ErrMsg)
			continue
		}

		list, ok := poll.Data.Data.([]interface{})
		// Validate the API response structure. If faiure are detected, requeue and hope it was a network blip.
		// ... else there might be breaking changes in either this client or in the API.
		if !ok {
			i.requeueAfter(tf, failureRequeueRate, fmt.Sprintf("ERROR api response in unexpected format %T", poll.Data.Data))
			continue
		}

		for _, item := range list {
			_, ok := item.(string)
			if !ok {
				i.requeueAfter(tf, failureRequeueRate, fmt.Sprintf("ERROR api response item in unexpected format %T", item))
				continue MainLoop
			}
			_, err := base64.StdEncoding.DecodeString(item.(string))
			if err != nil {
				i.requeueAfter(tf, failureRequeueRate, "ERROR api response item cannot be decoded")
				continue MainLoop
			}
		}

		isApplyReceivedResourcesFailed := false
		totalResourcesReceivedFromAPI := 0
		applyErr := ""
		for _, item := range list {
			b, _ := base64.StdEncoding.DecodeString(item.(string))

			var items corev1.List
			err = json.Unmarshal(b, &items)
			if err != nil {
				isApplyReceivedResourcesFailed = true
				applyErr += err.Error()
				continue
			}
			totalResourcesReceivedFromAPI += len(items.Items)

			err = applyRawManifest(ctx, kedge.KubernetesConfig(os.Getenv("KUBECONFIG")), b, namespace)
			if err != nil {
				isApplyReceivedResourcesFailed = true
				applyErr += err.Error()
				continue
			}
		}
		if isApplyReceivedResourcesFailed {
			i.requeueAfter(tf, failureRequeueRate, fmt.Sprintf("ERROR applying resources received: %s", applyErr))
			continue
		}
		log.Printf("Done handling workflow and received %d resources back from api \t(%s/%s)", totalResourcesReceivedFromAPI, namespace, name)
		if i.postJobContainerImage != "" {
			if err := i.createPostJob(namespace, name); err != nil {
				log.Printf("Error creating post job \t(%s/%s)", namespace, name)
			}
		}
	}
}

func (i informer) createPostJob(namespace, name string) error {
	serviceAccountConfig := corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      "tforc-post",
		},
	}
	serviceAccountClient := kubernetes.NewForConfigOrDie(i.config).CoreV1().ServiceAccounts(namespace)
	if _, err := serviceAccountClient.Create(i.ctx, &serviceAccountConfig, metav1.CreateOptions{}); err != nil {
		if !errors.IsAlreadyExists(err) {
			return err
		}
	}

	clusterRoleBinding := rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("tforc-post-binding-%s", namespace),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "tforc-post",
				Namespace: namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     "tforc",
			APIGroup: "rbac.authorization.k8s.io",
		},
	}
	clusterRoleBindingClient := kubernetes.NewForConfigOrDie(i.config).RbacV1().ClusterRoleBindings()
	if _, err := clusterRoleBindingClient.Create(i.ctx, &clusterRoleBinding, metav1.CreateOptions{}); err != nil {
		if !errors.IsAlreadyExists(err) {
			return err
		}
	}

	jobConfig := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    namespace,
			GenerateName: "tforc-post-",
			Labels: map[string]string{
				"galleybytes.com/tforc-post": "owned",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "tforc-post",
					RestartPolicy:      "Never",
					Containers: []corev1.Container{
						{
							Name:            "tforc-post",
							Image:           i.postJobContainerImage,
							ImagePullPolicy: "IfNotPresent",
							Env: []corev1.EnvVar{
								{
									Name:  "TFO_RESOURCE_NAME",
									Value: name,
								},
								{
									Name:  "TFO_RESOURCE_NAMESPACE",
									Value: namespace,
								},
							},
						},
					},
				},
			},
		},
	}

	jobClient := kubernetes.NewForConfigOrDie(i.config).BatchV1().Jobs(namespace)
	job, err := jobClient.Create(i.ctx, &jobConfig, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	log.Printf("Started Post Job %s/%s \t(%s/%s)", job.Namespace, job.Name, namespace, name)
	return nil

}

func (i informer) backgroundPostJobRemover() {
	go i.postJobRemover()
}

func (i informer) postJobRemover() {
	jobClient := kubernetes.NewForConfigOrDie(i.config).BatchV1().Jobs("")
	for {
		jobList, err := jobClient.List(i.ctx, metav1.ListOptions{
			LabelSelector: "galleybytes.com/tforc-post",
		})
		if err != nil {
			log.Println("Failed to list post jobs")
			continue
		}
		for _, job := range jobList.Items {
			if job.Status.Succeeded > 0 || job.Status.Failed > 0 {
				namespacedJobClient := kubernetes.NewForConfigOrDie(i.config).BatchV1().Jobs(job.Namespace)
				if err := namespacedJobClient.Delete(i.ctx, job.Name, metav1.DeleteOptions{}); err != nil {
					log.Printf("Failed to delete job %s/%s", job.Namespace, job.Name)
				} else {
					log.Printf("Removed job %s/%s", job.Namespace, job.Name)
				}
			}
		}
		time.Sleep(60 * time.Second)
	}
}

func applyRawManifest(c context.Context, config *rest.Config, raw []byte, namespace string) error {
	tempfile, err := os.CreateTemp(util.Tmpdir(), "*manifest")
	if err != nil {
		return err
	}
	defer os.Remove(tempfile.Name())

	err = os.WriteFile(tempfile.Name(), raw, 0755)
	if err != nil {
		return fmt.Errorf("whoa! error here: %s", err)
	}

	err = kedge.Apply(config, tempfile.Name(), namespace, []string{})
	if err != nil {
		return fmt.Errorf("error applying manifest: %s", err)
	}
	// Should this call block until the vcluster is up and running?
	return nil
}

func shouldPoll(tf tfv1beta1.Terraform) bool {
	if true {
		return true
	}
	return tf.Spec.OutputsSecret != ""
}

func (i informer) requeueAfter(tf tfv1beta1.Terraform, t time.Duration, msg string) {
	go func() {
		time.Sleep(t)
		i.queue.PushBack(tf)
	}()

	log.Printf(".... %s \t(%s/%s)", msg, tf.Namespace, tf.Name)

}
