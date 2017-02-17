/*
Copyright 2016 The Kubernetes Authors All rights reserved.

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

package cluster

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/docker/machine/drivers/virtualbox"
	"github.com/docker/machine/libmachine"
	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/engine"
	"github.com/docker/machine/libmachine/host"
	"github.com/docker/machine/libmachine/state"
	"github.com/golang/glog"
	"github.com/pkg/browser"
	"github.com/pkg/errors"
	"k8s.io/client-go/1.5/kubernetes"
	corev1 "k8s.io/client-go/1.5/kubernetes/typed/core/v1"
	kubeapi "k8s.io/client-go/1.5/pkg/api"
	"k8s.io/client-go/1.5/pkg/api/v1"
	"k8s.io/client-go/1.5/pkg/labels"
	"k8s.io/client-go/1.5/tools/clientcmd"

	"k8s.io/minikube/pkg/minikube/assets"
	"k8s.io/minikube/pkg/minikube/constants"
	"k8s.io/minikube/pkg/minikube/sshutil"
	"k8s.io/minikube/pkg/util"
)

var (
	certs = []string{"ca.crt", "ca.key", "apiserver.crt", "apiserver.key"}
)

const fileScheme = "file"

//This init function is used to set the logtostderr variable to false so that INFO level log info does not clutter the CLI
//INFO lvl logging is displayed due to the kubernetes api calling flag.Set("logtostderr", "true") in its init()
//see: https://github.com/kubernetes/kubernetes/blob/master/pkg/util/logs/logs.go#L32-34
func init() {
	flag.Set("logtostderr", "false")
}

// StartHost starts a host VM.
func StartHost(api libmachine.API, config MachineConfig) (*host.Host, error) {
	exists, err := api.Exists(constants.MachineName)
	if err != nil {
		return nil, errors.Wrapf(err, "Error checking if host exists: %s", constants.MachineName)
	}
	if !exists {
		return createHost(api, config)
	}

	glog.Infoln("Machine exists!")
	h, err := api.Load(constants.MachineName)
	if err != nil {
		return nil, errors.Wrap(err, "Error loading existing host. Please try running [minikube delete], then run [minikube start] again.")
	}

	s, err := h.Driver.GetState()
	glog.Infoln("Machine state: ", s)
	if err != nil {
		return nil, errors.Wrap(err, "Error getting state for host")
	}

	if s != state.Running {
		if err := h.Driver.Start(); err != nil {
			return nil, errors.Wrap(err, "Error starting stopped host")
		}
		if err := api.Save(h); err != nil {
			return nil, errors.Wrap(err, "Error saving started host")
		}
	}

	if err := h.ConfigureAuth(); err != nil {
		return nil, &util.RetriableError{Err: errors.Wrap(err, "Error configuring auth on host")}
	}
	return h, nil
}

// StopHost stops the host VM.
func StopHost(api libmachine.API) error {
	host, err := api.Load(constants.MachineName)
	if err != nil {
		return errors.Wrapf(err, "Error loading host: %s", constants.MachineName)
	}
	if err := host.Stop(); err != nil {
		return errors.Wrapf(err, "Error stopping host: %s", constants.MachineName)
	}
	return nil
}

// DeleteHost deletes the host VM.
func DeleteHost(api libmachine.API) error {
	host, err := api.Load(constants.MachineName)
	if err != nil {
		return errors.Wrapf(err, "Error deleting host: %s", constants.MachineName)
	}
	m := util.MultiError{}
	m.Collect(host.Driver.Remove())
	m.Collect(api.Remove(constants.MachineName))
	return m.ToError()
}

// GetHostStatus gets the status of the host VM.
func GetHostStatus(api libmachine.API) (string, error) {
	dne := "Does Not Exist"
	exists, err := api.Exists(constants.MachineName)
	if err != nil {
		return "", errors.Wrapf(err, "Error checking that api exists for: %s", constants.MachineName)
	}
	if !exists {
		return dne, nil
	}

	host, err := api.Load(constants.MachineName)
	if err != nil {
		return "", errors.Wrapf(err, "Error loading api for: %s", constants.MachineName)
	}

	s, err := host.Driver.GetState()
	if s.String() == "" {
		return dne, nil
	}
	if err != nil {
		return "", errors.Wrap(err, "Error getting host state")
	}
	return s.String(), nil
}

// GetLocalkubeStatus gets the status of localkube from the host VM.
func GetLocalkubeStatus(api libmachine.API) (string, error) {
	host, err := CheckIfApiExistsAndLoad(api)
	if err != nil {
		return "", err
	}
	s, err := host.RunSSHCommand(localkubeStatusCommand)
	if err != nil {
		return "", err
	}
	s = strings.TrimSpace(s)
	if state.Running.String() == s {
		return state.Running.String(), nil
	} else if state.Stopped.String() == s {
		return state.Stopped.String(), nil
	} else {
		return "", fmt.Errorf("Error: Unrecognize output from GetLocalkubeStatus: %s", s)
	}
}

type sshAble interface {
	RunSSHCommand(string) (string, error)
}

// StartCluster starts a k8s cluster on the specified Host.
func StartCluster(h sshAble, kubernetesConfig KubernetesConfig) error {
	startCommand, err := GetStartCommand(kubernetesConfig)
	if err != nil {
		return errors.Wrapf(err, "Error generating start command: %s", err)
	}
	glog.Infoln(startCommand)
	output, err := h.RunSSHCommand(startCommand)
	glog.Infoln(output)
	if err != nil {
		return errors.Wrapf(err, "Error running ssh command: %s", startCommand)
	}
	return nil
}

func UpdateCluster(h sshAble, d drivers.Driver, config KubernetesConfig) error {
	client, err := sshutil.NewSSHClient(d)
	if err != nil {
		return errors.Wrap(err, "Error creating new ssh client")
	}

	// transfer localkube from cache/asset to vm
	if localkubeURIWasSpecified(config) {
		lCacher := localkubeCacher{config}
		if err = lCacher.updateLocalkubeFromURI(client); err != nil {
			return errors.Wrap(err, "Error updating localkube from uri")
		}
	} else {
		if err = updateLocalkubeFromAsset(client); err != nil {
			return errors.Wrap(err, "Error updating localkube from asset")
		}
	}
	fileAssets := []assets.CopyableFile{}
	assets.AddMinikubeAddonsDirToAssets(&fileAssets)
	// merge files to copy
	var copyableFiles []assets.CopyableFile
	for _, addonBundle := range assets.Addons {
		if isEnabled, err := addonBundle.IsEnabled(); err == nil && isEnabled {
			for _, addon := range addonBundle.Assets {
				copyableFiles = append(copyableFiles, addon)
			}
		} else if err != nil {
			return err
		}
	}
	copyableFiles = append(copyableFiles, fileAssets...)
	// transfer files to vm
	for _, copyableFile := range copyableFiles {
		if err := sshutil.TransferFile(copyableFile, client); err != nil {
			return err
		}
	}
	return nil
}

func localkubeURIWasSpecified(config KubernetesConfig) bool {
	// see if flag is different than default -> it was passed by user
	return config.KubernetesVersion != constants.DefaultKubernetesVersion
}

// SetupCerts gets the generated credentials required to talk to the APIServer.
func SetupCerts(d drivers.Driver) error {
	localPath := constants.GetMinipath()
	ipStr, err := d.GetIP()
	if err != nil {
		return errors.Wrap(err, "Error getting ip from driver")
	}
	glog.Infoln("Setting up certificates for IP: %s", ipStr)

	ip := net.ParseIP(ipStr)
	caCert := filepath.Join(localPath, "ca.crt")
	caKey := filepath.Join(localPath, "ca.key")
	publicPath := filepath.Join(localPath, "apiserver.crt")
	privatePath := filepath.Join(localPath, "apiserver.key")
	if err := GenerateCerts(caCert, caKey, publicPath, privatePath, ip); err != nil {
		return errors.Wrap(err, "Error generating certs")
	}

	client, err := sshutil.NewSSHClient(d)
	if err != nil {
		return errors.Wrap(err, "Error creating new ssh client")
	}

	for _, cert := range certs {
		p := filepath.Join(localPath, cert)
		data, err := ioutil.ReadFile(p)
		if err != nil {
			return errors.Wrapf(err, "Error reading file: %s", p)
		}
		perms := "0644"
		if strings.HasSuffix(cert, ".key") {
			perms = "0600"
		}
		if err := sshutil.Transfer(bytes.NewReader(data), len(data), util.DefaultCertPath, cert, perms, client); err != nil {
			return errors.Wrapf(err, "Error transferring data: %s", string(data))
		}
	}
	return nil
}

func engineOptions(config MachineConfig) *engine.Options {

	o := engine.Options{
		Env:              config.DockerEnv,
		InsecureRegistry: config.InsecureRegistry,
		RegistryMirror:   config.RegistryMirror,
	}
	return &o
}

func createVirtualboxHost(config MachineConfig) drivers.Driver {
	d := virtualbox.NewDriver(constants.MachineName, constants.GetMinipath())
	d.Boot2DockerURL = config.Downloader.GetISOFileURI(config.MinikubeISO)
	d.Memory = config.Memory
	d.CPU = config.CPUs
	d.DiskSize = int(config.DiskSize)
	d.HostOnlyCIDR = config.HostOnlyCIDR
	return d
}

func createHost(api libmachine.API, config MachineConfig) (*host.Host, error) {
	var driver interface{}

	if err := config.Downloader.CacheMinikubeISOFromURL(config.MinikubeISO); err != nil {
		return nil, errors.Wrap(err, "Error attempting to cache minikube ISO from URL")
	}

	switch config.VMDriver {
	case "virtualbox":
		driver = createVirtualboxHost(config)
	case "vmwarefusion":
		driver = createVMwareFusionHost(config)
	case "kvm":
		driver = createKVMHost(config)
	case "xhyve":
		driver = createXhyveHost(config)
	case "hyperv":
		driver = createHypervHost(config)
	default:
		glog.Exitf("Unsupported driver: %s\n", config.VMDriver)
	}

	data, err := json.Marshal(driver)
	if err != nil {
		return nil, errors.Wrap(err, "Error marshalling json")
	}

	h, err := api.NewHost(config.VMDriver, data)
	if err != nil {
		return nil, errors.Wrap(err, "Error creating new host")
	}

	h.HostOptions.AuthOptions.CertDir = constants.GetMinipath()
	h.HostOptions.AuthOptions.StorePath = constants.GetMinipath()
	h.HostOptions.EngineOptions = engineOptions(config)

	if err := api.Create(h); err != nil {
		// Wait for all the logs to reach the client
		time.Sleep(2 * time.Second)
		return nil, errors.Wrap(err, "Error creating host")
	}

	if err := api.Save(h); err != nil {
		return nil, errors.Wrap(err, "Error attempting to save")
	}
	return h, nil
}

// GetHostDockerEnv gets the necessary docker env variables to allow the use of docker through minikube's vm
func GetHostDockerEnv(api libmachine.API) (map[string]string, error) {
	host, err := CheckIfApiExistsAndLoad(api)
	if err != nil {
		return nil, errors.Wrap(err, "Error checking that api exists and loading it")
	}
	ip, err := host.Driver.GetIP()
	if err != nil {
		return nil, errors.Wrap(err, "Error getting ip from host")
	}

	tcpPrefix := "tcp://"
	port := "2376"

	envMap := map[string]string{
		"DOCKER_TLS_VERIFY": "1",
		"DOCKER_HOST":       tcpPrefix + net.JoinHostPort(ip, port),
		"DOCKER_CERT_PATH":  constants.MakeMiniPath("certs"),
	}
	return envMap, nil
}

// GetHostLogs gets the localkube logs of the host VM.
func GetHostLogs(api libmachine.API) (string, error) {
	host, err := CheckIfApiExistsAndLoad(api)
	if err != nil {
		return "", errors.Wrap(err, "Error checking that api exists and loading it")
	}
	s, err := host.RunSSHCommand(logsCommand)
	if err != nil {
		return "", err
	}
	return s, nil
}

func CheckIfApiExistsAndLoad(api libmachine.API) (*host.Host, error) {
	exists, err := api.Exists(constants.MachineName)
	if err != nil {
		return nil, errors.Wrapf(err, "Error checking that api exists for: %s", constants.MachineName)
	}
	if !exists {
		return nil, errors.Errorf("Machine does not exist for api.Exists(%s)", constants.MachineName)
	}

	host, err := api.Load(constants.MachineName)
	if err != nil {
		return nil, errors.Wrapf(err, "Error loading api for: %s", constants.MachineName)
	}
	return host, nil
}

func CreateSSHShell(api libmachine.API, args []string) error {
	host, err := CheckIfApiExistsAndLoad(api)
	if err != nil {
		return errors.Wrap(err, "Error checking if api exist and loading it")
	}

	currentState, err := host.Driver.GetState()
	if err != nil {
		return errors.Wrap(err, "Error getting state of host")
	}

	if currentState != state.Running {
		return errors.Errorf("Error: Cannot run ssh command: Host %q is not running", constants.MachineName)
	}

	client, err := host.CreateSSHClient()
	if err != nil {
		return errors.Wrap(err, "Error creating ssh client")
	}
	return client.Shell(strings.Join(args, " "))
}

func GetServiceURLsForService(api libmachine.API, namespace, service string, t *template.Template) ([]string, error) {
	host, err := CheckIfApiExistsAndLoad(api)
	if err != nil {
		return nil, errors.Wrap(err, "Error checking if api exist and loading it")
	}

	ip, err := host.Driver.GetIP()
	if err != nil {
		return nil, errors.Wrap(err, "Error getting ip from host")
	}

	client, err := GetKubernetesClient()
	if err != nil {
		return nil, err
	}

	return getServiceURLsWithClient(client, ip, namespace, service, t)
}

func getServiceURLsWithClient(client *kubernetes.Clientset, ip, namespace, service string, t *template.Template) ([]string, error) {
	if t == nil {
		return nil, errors.New("Error, attempted to generate service url with nil --format template")
	}

	ports, err := getServicePorts(client, namespace, service)
	if err != nil {
		return nil, err
	}
	urls := []string{}
	for _, port := range ports {

		var doc bytes.Buffer
		err = t.Execute(&doc, struct {
			IP   string
			Port int32
		}{
			ip,
			port,
		})
		if err != nil {
			return nil, err
		}

		u, err := url.Parse(doc.String())
		if err != nil {
			return nil, err
		}

		urls = append(urls, u.String())
	}
	return urls, nil
}

type serviceGetter interface {
	Get(name string) (*v1.Service, error)
	List(kubeapi.ListOptions) (*v1.ServiceList, error)
}

func getServicePorts(client *kubernetes.Clientset, namespace, service string) ([]int32, error) {
	services := client.Services(namespace)
	return getServicePortsFromServiceGetter(services, service)
}

type MissingNodePortError struct {
	service *v1.Service
}

func (e MissingNodePortError) Error() string {
	return fmt.Sprintf("Service %s/%s does not have a node port. To have one assigned automatically, the service type must be NodePort or LoadBalancer, but this service is of type %s.", e.service.Namespace, e.service.Name, e.service.Spec.Type)
}

func getServiceFromServiceGetter(services serviceGetter, service string) (*v1.Service, error) {
	svc, err := services.Get(service)
	if err != nil {
		return nil, fmt.Errorf("Error getting %s service: %s", service, err)
	}
	return svc, nil
}

func getServicePortsFromServiceGetter(services serviceGetter, service string) ([]int32, error) {
	svc, err := getServiceFromServiceGetter(services, service)
	if err != nil {
		return nil, err
	}
	var nodePorts []int32
	if len(svc.Spec.Ports) > 0 {
		for _, port := range svc.Spec.Ports {
			if port.NodePort > 0 {
				nodePorts = append(nodePorts, port.NodePort)
			}
		}
	}
	if len(nodePorts) == 0 {
		return nil, MissingNodePortError{svc}
	}
	return nodePorts, nil
}

func GetKubernetesClient() (*kubernetes.Clientset, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("Error creating kubeConfig: %s", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "Error creating new client from kubeConfig.ClientConfig()")
	}
	return client, nil
}

// EnsureMinikubeRunningOrExit checks that minikube has a status available and that
// that the status is `Running`, otherwise it will exit
func EnsureMinikubeRunningOrExit(api libmachine.API, exitStatus int) {
	s, err := GetHostStatus(api)
	if err != nil {
		glog.Errorln("Error getting machine status:", err)
		os.Exit(1)
	}
	if s != state.Running.String() {
		fmt.Fprintln(os.Stdout, "minikube is not currently running so the service cannot be accessed")
		os.Exit(exitStatus)
	}
}

type ServiceURL struct {
	Namespace string
	Name      string
	URLs      []string
}

type ServiceURLs []ServiceURL

func GetServiceURLs(api libmachine.API, namespace string, t *template.Template) (ServiceURLs, error) {
	host, err := CheckIfApiExistsAndLoad(api)
	if err != nil {
		return nil, err
	}

	ip, err := host.Driver.GetIP()
	if err != nil {
		return nil, err
	}

	client, err := GetKubernetesClient()
	if err != nil {
		return nil, err
	}

	getter := client.Services(namespace)

	svcs, err := getter.List(kubeapi.ListOptions{})
	if err != nil {
		return nil, err
	}

	var serviceURLs []ServiceURL

	for _, svc := range svcs.Items {
		urls, err := getServiceURLsWithClient(client, ip, svc.Namespace, svc.Name, t)
		if err != nil {
			if _, ok := err.(MissingNodePortError); ok {
				serviceURLs = append(serviceURLs, ServiceURL{Namespace: svc.Namespace, Name: svc.Name})
				continue
			}
			return nil, err
		}
		serviceURLs = append(serviceURLs, ServiceURL{Namespace: svc.Namespace, Name: svc.Name, URLs: urls})
	}

	return serviceURLs, nil
}

// CheckService waits for the specified service to be ready by returning an error until the service is up
// The check is done by polling the endpoint associated with the service and when the endpoint exists, returning no error->service-online
func CheckService(namespace string, service string) error {
	client, err := GetKubernetesClient()
	if err != nil {
		return &util.RetriableError{Err: err}
	}
	endpoints := client.Endpoints(namespace)
	if err != nil {
		return &util.RetriableError{Err: err}
	}
	endpoint, err := endpoints.Get(service)
	if err != nil {
		return &util.RetriableError{Err: err}
	}
	return checkEndpointReady(endpoint)
}

func checkEndpointReady(endpoint *v1.Endpoints) error {
	const notReadyMsg = "Waiting, endpoint for service is not ready yet...\n"
	if len(endpoint.Subsets) == 0 {
		fmt.Fprintf(os.Stderr, notReadyMsg)
		return &util.RetriableError{Err: errors.New("Endpoint for service is not ready yet")}
	}
	for _, subset := range endpoint.Subsets {
		if len(subset.Addresses) == 0 {
			fmt.Fprintf(os.Stderr, notReadyMsg)
			return &util.RetriableError{Err: errors.New("No endpoints for service are ready yet")}
		}
	}
	return nil
}

func WaitAndMaybeOpenService(api libmachine.API, namespace string, service string, urlTemplate *template.Template, urlMode bool, https bool) {
	if err := util.RetryAfter(20, func() error { return CheckService(namespace, service) }, 6*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "Could not find finalized endpoint being pointed to by %s: %s\n", service, err)
		os.Exit(1)
	}

	urls, err := GetServiceURLsForService(api, namespace, service, urlTemplate)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "Check that minikube is running and that you have specified the correct namespace (-n flag).")
		os.Exit(1)
	}
	for _, url := range urls {
		if https {
			url = strings.Replace(url, "http", "https", 1)
		}
		if urlMode || !strings.HasPrefix(url, "http") {
			fmt.Fprintln(os.Stdout, url)
		} else {
			fmt.Fprintln(os.Stdout, "Opening kubernetes service "+namespace+"/"+service+" in default browser...")
			browser.OpenURL(url)
		}
	}
}

func GetServiceListByLabel(namespace string, key string, value string) (*v1.ServiceList, error) {
	client, err := GetKubernetesClient()
	if err != nil {
		return &v1.ServiceList{}, &util.RetriableError{Err: err}
	}
	services := client.Services(namespace)
	if err != nil {
		return &v1.ServiceList{}, &util.RetriableError{Err: err}
	}
	return getServiceListFromServicesByLabel(services, key, value)
}

func getServiceListFromServicesByLabel(services corev1.ServiceInterface, key string, value string) (*v1.ServiceList, error) {
	selector := labels.SelectorFromSet(labels.Set(map[string]string{key: value}))
	serviceList, err := services.List(kubeapi.ListOptions{LabelSelector: selector})
	if err != nil {
		return &v1.ServiceList{}, &util.RetriableError{Err: err}
	}

	return serviceList, nil
}

// CreateSecret creates or modifies secrets
func CreateSecret(namespace, name string, dataValues map[string]string, labels map[string]string) error {
	client, err := GetKubernetesClient()
	if err != nil {
		return &util.RetriableError{Err: err}
	}
	secrets := client.Secrets(namespace)
	if err != nil {
		return &util.RetriableError{Err: err}
	}

	secret, _ := secrets.Get(name)

	// Delete existing secret
	if len(secret.Name) > 0 {
		err = DeleteSecret(namespace, name)
		if err != nil {
			return &util.RetriableError{Err: err}
		}
	}

	// convert strings to data secrets
	data := map[string][]byte{}
	for key, value := range dataValues {
		data[key] = []byte(value)
	}

	// Create Secret
	secretObj := &v1.Secret{
		ObjectMeta: v1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Data: data,
		Type: v1.SecretTypeOpaque,
	}

	_, err = secrets.Create(secretObj)
	if err != nil {
		fmt.Println("err: ", err)
		return &util.RetriableError{Err: err}
	}

	return nil
}

// DeleteSecret deletes a secret from a namespace
func DeleteSecret(namespace, name string) error {
	client, err := GetKubernetesClient()
	if err != nil {
		return &util.RetriableError{Err: err}
	}

	secrets := client.Secrets(namespace)
	if err != nil {
		return &util.RetriableError{Err: err}
	}

	err = secrets.Delete(name, &kubeapi.DeleteOptions{})
	if err != nil {
		return &util.RetriableError{Err: err}
	}

	return nil
}
