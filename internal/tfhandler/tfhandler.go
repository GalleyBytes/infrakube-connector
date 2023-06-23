package tfhandler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/galleybytes/monitor/projects/terraform-operator-remote-controller/pkg/tfoapiclient"
	tfv1beta1 "github.com/galleybytes/terraform-operator/pkg/apis/tf/v1beta1"
	"github.com/gammazero/deque"
	gocache "github.com/patrickmn/go-cache"
	corev1 "k8s.io/api/core/v1"
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
		Resource: "terraforms",
	}
)

type informer struct {
	dynamicinformer.DynamicSharedInformerFactory
	ctx         context.Context
	config      *rest.Config
	clientset   *tfoapiclient.Clientset
	cache       *gocache.Cache
	queue       *deque.Deque[tfv1beta1.Terraform]
	clusterName string
}

func NewInformer(config *rest.Config, clientSetup tfoapiclient.ClientSetup, host, user, password string, insecureSkipVerify bool) informer {
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

	tfhandler := informer{
		ctx:         context.TODO(),
		config:      config,
		clientset:   clientset,
		clusterName: clientSetup.ClusterName,
		cache:       gocache.New(10*time.Minute, 10*time.Minute),
		queue:       &deque.Deque[tfv1beta1.Terraform]{},
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
	i.Start(stopCh)
	<-stopCh
	log.Println("Stopped informer")
}

func (i informer) addEvent(obj interface{}) {
	tf, err := assertTf(obj)
	if err != nil {
		log.Printf("ERROR in add event: %s", err)
		return
	}
	log.Printf("Add event observed '%s'", tf.Name)

	err = i.SyncDependencies(tf)
	if err != nil {
		log.Println(err.Error())
		return
	}

	postResult, err := i.clientset.Cluster(i.clusterName).Event().Create(context.TODO(), tf)
	if err != nil {
		log.Println(err.Error())
		return
	}

	if postResult.IsSuccess {
		return
	}

	if !strings.Contains(postResult.ErrMsg, "TFOResource already exists") {
		log.Println(postResult.ErrMsg)
		return
	}

	putResult, err := i.clientset.Cluster(i.clusterName).Event().Update(context.TODO(), tf)
	if err != nil {
		log.Printf("ERROR in request to add resource: %s", err)
		return
	}

	if !putResult.IsSuccess {
		log.Printf("ERROR %s", putResult.ErrMsg)
		return
	}

	i.queue.PushBack(*tf)
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

	err = i.SyncDependencies(tfnew)
	if err != nil {
		log.Println(err.Error())
		return
	}

	postResult, err := i.clientset.Cluster(i.clusterName).Event().Create(context.TODO(), tfnew)
	if err != nil {
		log.Println(err.Error())
		return
	}

	if postResult.IsSuccess {
		return
	}

	if !strings.Contains(postResult.ErrMsg, "TFOResource already exists") {
		log.Println(postResult.ErrMsg)
		return
	}

	putResult, err := i.clientset.Cluster(i.clusterName).Event().Update(context.TODO(), tfnew)
	if err != nil {
		log.Printf("ERROR in request to add resource: %s", err)
		return
	}

	if !putResult.IsSuccess {
		log.Printf("ERROR %s", putResult.ErrMsg)
		return
	}

	i.queue.PushBack(*tfnew)
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

func assertTf(obj interface{}) (*tfv1beta1.Terraform, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var tf tfv1beta1.Terraform
	err = json.Unmarshal(b, &tf)
	if err != nil {
		return nil, err
	}
	return &tf, nil
}

// gatherDependenciesToSync will read the tf config and find k8s resources that exist on the originating cluster.
// The resources found are requirements to execute the terraform workflow. Dependencies (resources) will be sent to
// the hub cluster (via the tfo-api) so the workflow can access these resources when called upon.
func (i informer) gatherDependenciesToSync(tf *tfv1beta1.Terraform) (*corev1.List, error) {
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

	for tf.Spec.TerraformModule.ConfigMapSelector != nil {
		// terraformmodule.configmapselector
		configMapNames = append(configMapNames, tf.Spec.TerraformModule.ConfigMapSelector.Name)
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
	}

	for _, configMapName := range configMapNames {
		configMap, err := configMapClient.Get(i.ctx, configMapName, metav1.GetOptions{})
		if err != nil {
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
	}

	return &resourceList, nil
}

func (i informer) SyncDependencies(tf *tfv1beta1.Terraform) error {
	// for remote operation to work, the hub cluster must "look-like" the originating cluster. To do so
	// all the resources that a workflow depends on (ie secrets and configmaps) must be synced to the
	// hub cluster (ie the vcluster).
	dependencies, err := i.gatherDependenciesToSync(tf)
	if err != nil {
		return fmt.Errorf("An error occurred gathering dependencies from '%s/%s': %s\n", tf.Namespace, tf.Name, err.Error())
	}
	data := map[string][]byte{
		"raw":       raw(dependencies),
		"namespace": []byte(tf.Namespace),
	}
	syncResult, err := i.clientset.Cluster(i.clusterName).SyncDependencies().Update(context.TODO(), data)
	if err != nil {
		return err
	}
	if !syncResult.IsSuccess {
		return fmt.Errorf(syncResult.ErrMsg)
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
