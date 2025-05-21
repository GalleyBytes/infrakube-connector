package tfhandler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/galleybytes/infrakube-connector/pkg/tfoapiclient"
	tfv1beta1 "github.com/galleybytes/infrakube/pkg/apis/infra3/v1"
	"github.com/gammazero/deque"
	gocache "github.com/patrickmn/go-cache"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

var (
	terraformResource = schema.GroupVersionResource{
		Group:    tfv1beta1.SchemeGroupVersion.Group,
		Version:  tfv1beta1.SchemeGroupVersion.Version,
		Resource: "tfs",
	}
)

type informer struct {
	dynamicinformer.DynamicSharedInformerFactory
	ctx                   context.Context
	config                *rest.Config
	clientset             *tfoapiclient.Clientset
	cache                 *gocache.Cache
	queue                 *deque.Deque[tfv1beta1.Tf]
	postJobContainerImage string
	postJobTolerations    []byte
	clusterName           string
}

func NewInformer(config *rest.Config, clientSetup tfoapiclient.ClientSetup, host, user, password string, insecureSkipVerify bool, postJobContainerImage string, postJobTolerations []byte) informer {
	log.Println("Setting up")
	dynamicClient := dynamic.NewForConfigOrDie(config)
	clientset, err := tfoapiclient.NewClientset(host, user, password, insecureSkipVerify)
	if err != nil {
		log.Fatal(err)
	}

	// One time registration of the cluster. Returns success if already exist or if is created successfully.
	err = clientset.Registration().Register(clientSetup)
	if err != nil {
		log.Fatal(err)
	}

	// A first time vcluster can take a minute to be available. Wait until the vcluster is available.
	vclusterReady := false
	for {
		if !vclusterReady {
			clusterHealthResult, err := clientset.Cluster(clientSetup.ClusterName).Health().Read(context.TODO(), nil)
			if err != nil || !clusterHealthResult.IsSuccess {
				time.Sleep(60 * time.Second)
				continue
			}
			vclusterReady = true
			log.Println("VCluster is ready!")
		}

		tfoHealthResult, err := clientset.Cluster(clientSetup.ClusterName).TFOHealth().Read(context.TODO(), nil)
		if err != nil || !tfoHealthResult.IsSuccess {
			time.Sleep(60 * time.Second)
			continue
		}
		log.Println("TFO is ready!")
		// Both vcluster and tfo are ready
		break
	}

	tfhandler := informer{
		ctx:                   context.TODO(),
		config:                config,
		clientset:             clientset,
		clusterName:           clientSetup.ClusterName,
		cache:                 gocache.New(10*time.Minute, 10*time.Minute),
		queue:                 &deque.Deque[tfv1beta1.Tf]{},
		postJobContainerImage: postJobContainerImage,
		postJobTolerations:    postJobTolerations,
	}

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    tfhandler.addEvent,
		UpdateFunc: tfhandler.updateEvent,
		DeleteFunc: tfhandler.deleteEvent,
	}
	informer := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 0)
	informer.ForResource(terraformResource).Informer().AddEventHandler(handler)
	tfhandler.DynamicSharedInformerFactory = informer
	return tfhandler
}

func (i informer) Run() {
	log.Println("Starting informer")
	stopCh := make(chan struct{})
	defer close(stopCh)
	i.backgroundQueueWorker()
	i.backgroundPostJobRemover()

	// Start the informer
	i.Start(stopCh)
	<-stopCh
	log.Println("Stopped informer")
}

type eventQueryRetryResult struct {
	next bool
	err  error
}

// Send a query to the API and only return upon a successful response from the API withith a given time threshed.
// Upon reaching the threshold, exit the program to prevent the tf resouce on the API (vcluster) from being
// out-of-sync with the local resource
func eventQueryRetrier(crud tfoapiclient.CrudResource, crudType string, data any) *eventQueryRetryResult {
	expiration := time.Now().Add(time.Duration(60 * time.Minute))
	done := make(chan eventQueryRetryResult)

	go func() {
		for {
			var result *tfoapiclient.Result
			var err error
			switch crudType {
			case "CREATE":
				result, err = crud.Create(context.TODO(), data)
			case "UPDATE":
				result, err = crud.Update(context.TODO(), data)
			case "READ":
				result, err = crud.Read(context.TODO(), data)
			default:
				done <- eventQueryRetryResult{false, fmt.Errorf("verb '%s' undefined", crudType)}
				return
			}

			if err == nil {
				if result.IsSuccess && result.ErrMsg == "" {
					done <- eventQueryRetryResult{false, nil}
					return
				} else if result.IsSuccess {
					done <- eventQueryRetryResult{true, fmt.Errorf(result.ErrMsg)}
					return
				}
			}
			if time.Now().After(expiration) {
				// After this expiration, fatally terminate the controller to prevent silent errors
				if err != nil {
					log.Fatal(err)
				}
				log.Fatal(result.ErrMsg)
			}
			if err != nil {
				log.Println(err)
			} else {
				// Case where err is not nil but the result was not successful
				log.Println(result.ErrMsg)
			}

			time.Sleep(20 * time.Second)
		}
	}()

	e := <-done
	return &e

}

func (i informer) createAndUpdate(tf *tfv1beta1.Tf) {
	log.Printf("=> Gathering resources to sync \t(%s/%s)", tf.Namespace, tf.Name)
	err := i.SyncDependencies(tf)
	if err != nil {
		log.Println(err.Error())
		return
	}
	for _, action := range []string{"CREATE", "UPDATE"} {
		result := eventQueryRetrier(i.clientset.Cluster(i.clusterName).Event(), action, tf)
		if result == nil {
			log.Printf("Unknown error occurred in %s \t(%s/%s)\n", action, tf.Namespace, tf.Name)
			return
		}
		if !result.next {
			if result.err != nil {
				log.Println(result.err.Error())
				return
			}
			log.Printf("===> %sD tf resource \t(%s/%s)\n", action, tf.Namespace, tf.Name)
			break
		}
	}
	go func() {
		// Give time for the API to process before queueing up the worker that polls for results
		time.Sleep(30 * time.Second)
		i.queue.PushBack(*tf)
	}()
}

func (i informer) addEvent(obj interface{}) {
	tf, err := assertTf(obj)
	if err != nil {
		log.Printf("ERROR in add event: %s", err)
		return
	}
	log.Printf("Add event observed for tf resource \t(%s/%s)", tf.Namespace, tf.Name)
	i.createAndUpdate(tf)
}

func (i informer) updateEvent(old, new interface{}) {
	tfold, err := assertTf(old)
	if err != nil {
		log.Printf("ERROR in update event: %s", err)
		return
	}
	tfnew, err := assertTf(new)
	if err != nil {
		log.Printf("ERROR in update event: %s", err)
		return
	}
	if tfold.Generation != tfnew.Generation {
		log.Println("Generation change: ", tfnew.Name)
	} else {
		log.Println("Observed update event: ", tfnew.Name)
	}

	i.createAndUpdate(tfnew)
}

func (i informer) deleteEvent(obj interface{}) {
	tf, err := assertTf(obj)
	if err != nil {
		log.Printf("ERROR in delete event: %s", err)
		return
	}
	_ = tf
	log.Println("delete event")
}

func assertTf(obj interface{}) (*tfv1beta1.Tf, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var tf tfv1beta1.Tf
	err = json.Unmarshal(b, &tf)
	if err != nil {
		return nil, err
	}
	return &tf, nil
}

// gatherDependenciesToSync will read the tf config and find k8s resources that exist on the originating cluster.
// The resources found are requirements to execute the terraform workflow. Dependencies (resources) will be sent to
// the hub cluster (via the tfo-api) so the workflow can access these resources when called upon.
func (i informer) gatherDependenciesToSync(tf *tfv1beta1.Tf) (*corev1.List, error) {
	resourceList := corev1.List{}
	resourceList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "List",
	})
	secretNames := []string{}
	configMapNames := []string{}
	secretClient := kubernetes.NewForConfigOrDie(i.config).CoreV1().Secrets(tf.Namespace)
	configMapClient := kubernetes.NewForConfigOrDie(i.config).CoreV1().ConfigMaps(tf.Namespace)

	for _, c := range tf.Spec.Credentials {
		// credentials.secretNameRef
		if c.SecretNameRef.Name != "" {
			secretNames = append(secretNames, c.SecretNameRef.Name)
		}
	}

	for _, c := range tf.Spec.SCMAuthMethods {
		// scmauthmethods[].git.ssh.sshkeysecretref
		// scmauthmethods[].git.https.tokensecretref
		if c.Git != nil {
			if c.Git.SSH != nil {
				if c.Git.SSH.SSHKeySecretRef != nil {
					secretNames = append(secretNames, c.Git.SSH.SSHKeySecretRef.Name)
				}
			}
			if c.Git.HTTPS != nil {
				if c.Git.HTTPS.TokenSecretRef != nil {
					secretNames = append(secretNames, c.Git.HTTPS.TokenSecretRef.Name)
				}
			}
		}
	}

	if tf.Spec.SSHTunnel != nil {
		// sshtunnel.sshkeysecretref
		secretNames = append(secretNames, tf.Spec.SSHTunnel.SSHKeySecretRef.Name)
	}

	if tf.Spec.TfModule.ConfigMapSelector != nil {
		// terraformmodule.configmapselector
		configMapNames = append(configMapNames, tf.Spec.TfModule.ConfigMapSelector.Name)
	}

	for _, c := range tf.Spec.TaskOptions {
		for _, e := range c.Env {
			// taskoptions[].env[].valuefrom.configmapkeyref
			// taskoptions[].env[].valuefrom.secretkeyref
			if e.ValueFrom != nil {
				if e.ValueFrom.SecretKeyRef != nil {
					secretNames = append(secretNames, e.ValueFrom.SecretKeyRef.Name)
				}
				if e.ValueFrom.ConfigMapKeyRef != nil {
					configMapNames = append(configMapNames, e.ValueFrom.ConfigMapKeyRef.Name)
				}
			}
		}
		for _, e := range c.EnvFrom {
			// taskoptions[].envfrom[].configmapref
			// taskoptions[].envfrom[].secretref
			if e.SecretRef != nil {
				secretNames = append(secretNames, e.SecretRef.Name)
			}
			if e.ConfigMapRef != nil {
				configMapNames = append(configMapNames, e.ConfigMapRef.Name)
			}
		}
		if c.Script.ConfigMapSelector != nil {
			// taskoptions[].script.configmapselector
			configMapNames = append(configMapNames, c.Script.ConfigMapSelector.Name)
		}
	}

	for _, secretName := range secretNames {
		secret, err := secretClient.Get(i.ctx, secretName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, err
		}

		gvks, _, err := scheme.Scheme.ObjectKinds(secret)
		if err != nil {
			return nil, err
		}
		if len(gvks) == 0 {
			return nil, err
		}
		secret.SetGroupVersionKind(gvks[0])

		buf := bytes.NewBuffer([]byte{})
		k8sjson.NewSerializer(k8sjson.DefaultMetaFactory, runtime.NewScheme(), runtime.NewScheme(), true).Encode(secret, buf)

		// tf.Spec.
		resourceList.Items = append(resourceList.Items, runtime.RawExtension{
			Raw:    buf.Bytes(),
			Object: secret,
		})
		buf.Reset()
		log.Printf("==> syncing secret/%s \t(%s/%s)", secretName, tf.Namespace, tf.Name)
	}

	for _, configMapName := range configMapNames {
		configMap, err := configMapClient.Get(i.ctx, configMapName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, err
		}

		gvks, _, err := scheme.Scheme.ObjectKinds(configMap)
		if err != nil {
			return nil, err
		}
		if len(gvks) == 0 {
			return nil, err
		}
		configMap.SetGroupVersionKind(gvks[0])

		buf := bytes.NewBuffer([]byte{})
		k8sjson.NewSerializer(k8sjson.DefaultMetaFactory, runtime.NewScheme(), runtime.NewScheme(), true).Encode(configMap, buf)

		// tf.Spec.
		resourceList.Items = append(resourceList.Items, runtime.RawExtension{
			Raw:    buf.Bytes(),
			Object: configMap,
		})
		buf.Reset()
		log.Printf("==> syncing configmap/%s \t(%s/%s)", configMapName, tf.Namespace, tf.Name)
	}

	return &resourceList, nil
}

func (i informer) SyncDependencies(tf *tfv1beta1.Tf) error {
	// for remote operation to work, the hub cluster must "look-like" the originating cluster. To do so
	// all the resources that a workflow depends on (ie secrets and configmaps) must be synced to the
	// hub cluster (ie the vcluster).
	dependencies, err := i.gatherDependenciesToSync(tf)
	if err != nil {
		return fmt.Errorf("an error occurred gathering dependencies from '%s/%s': %s", tf.Namespace, tf.Name, err.Error())
	}
	data := map[string][]byte{
		"raw":       raw(dependencies),
		"namespace": []byte(tf.Namespace),
	}
	result := eventQueryRetrier(i.clientset.Cluster(i.clusterName).SyncDependencies(), "UPDATE", data)
	if result == nil {

		return fmt.Errorf("an unknown error has occurred")
	}
	if result.err != nil {
		return result.err
	}

	return nil
}

func raw(o interface{}) []byte {
	b, err := json.Marshal(o)
	if err != nil {
		log.Panic(err)
	}
	var out bytes.Buffer
	err = json.Indent(&out, b, "", "  ")
	if err != nil {
		log.Panic(err)
	}
	return out.Bytes()
}
