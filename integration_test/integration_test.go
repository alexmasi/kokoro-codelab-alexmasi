package integration_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	dclient "github.com/docker/docker/client"
	git "github.com/go-git/go-git/v5"
	"github.com/golang/protobuf/proto"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/openconfig/gnmi/client"
	gpb "github.com/openconfig/gnmi/proto/gnmi"
	log "github.com/sirupsen/logrus"
	"keysight.com/otgclient"

	_ "github.com/openconfig/gnmi/client/gnmi"
)

const (
	kneRepo      = "https://github.com/google/kne"
	meshnetRepo  = "https://github.com/networkop/meshnet-cni"
	keysightRepo = "https://source.developers.google.com/p/gep-kne/r/keysight"

	testTag = "test"

	lbNamespaceURL = "https://raw.githubusercontent.com/metallb/metallb/master/manifests/namespace.yaml"
	lbURL          = "https://raw.githubusercontent.com/metallb/metallb/master/manifests/metallb.yaml"
	ateMapTemplate = "%s:5555+%s:50071"
)

var (
	baseTmpDir = filepath.Join("/", "tmp", "kne-test")

	testTmpDir  string
	kneCLIPath  string
	kneDir      string
	meshnetDir  string
	keysightDir string

	// containers maps an abbreviated name of the container to its full path.
	containers = map[string]string{
		"meshnet":           "networkop/meshnet",
		"init-wait":         "networkop/init-wait",
		"alpine":            "alpine",
		"ceos":              "us-west1-docker.pkg.dev/gep-kne/arista/ceos",
		"athena-controller": "us-west1-docker.pkg.dev/gep-kne/keysight/athena/ixia-c-controller",
		"athena-engine":     "us-west1-docker.pkg.dev/gep-kne/keysight/athena/ixia-c-traffic-engine",
		"athena-protocols":  "us-west1-docker.pkg.dev/gep-kne/keysight/athena/l23_protocols",
		"athena-operator":   "us-west1-docker.pkg.dev/gep-kne/keysight/athena/operator",
		"athena-server":     "us-west1-docker.pkg.dev/gep-kne/keysight/athena/otg-gnmi-server",
	}
)

var (
	// kneVer              = flag.String("kne_ver", "", "Version of the KNE CLI to use, leave blank to use latest.")
	ceosVer             = flag.String("ceos_ver", "ga", "Version of the Arista Ceos container to use, leave blank to use latest.")
	athenaControllerVer = flag.String("athena_controller_ver", "ga", "Version of the Ixia Athena controller container to use, leave blank to use latest.")
	athenaEngineVer     = flag.String("athena_engine_ver", "ga", "Version of the Ixia Athena engine container to use, leave blank to use latest.")
	athenaProtocolsVer  = flag.String("athena_protocols_ver", "ga", "Version of the Ixia Athena protocols container to use, leave blank to use latest.")
	athenaOperatorVer   = flag.String("athena_operator_ver", "ga", "Version of the Ixia Athena operator container to use, leave blank to use latest.")
	athenaServerVer     = flag.String("athena_server_ver", "ga", "Version of the Ixia Athena server container to use, leave blank to use latest.")
)

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		log.Errorf("Failed setup: %v", err)
		teardown()
		os.Exit(1)
	}
	code := m.Run()
	teardown()
	os.Exit(code)
}

func setup() error {
	fmt.Println("setup")
	ctx := context.Background()
	if err := validateDeps(); err != nil {
		return fmt.Errorf("unable to validate required dependencies: %v", err)
	}
	if err := os.MkdirAll(baseTmpDir, 0700); err != nil {
		return err
	}
	var err error
	testTmpDir, err = ioutil.TempDir(baseTmpDir, "")
	if err != nil {
		return err
	}
	if err := cloneRepos(testTmpDir); err != nil {
		return fmt.Errorf("unable to clone required repos: %v", err)
	}
	if err := execCmdDir(filepath.Join(testTmpDir, "kne", "kne_cli"), "go", "build"); err != nil {
		return err
	}
	kneCLIPath = filepath.Join(testTmpDir, "kne", "kne_cli", "kne_cli")
	if _, err := os.Stat(kneCLIPath); os.IsNotExist(err) {
		return fmt.Errorf("kne_cli does not exist at path %q: %v", kneCLIPath, err)
	}
	dc, err := dclient.NewClientWithOpts(dclient.FromEnv)
	if err != nil {
		return err
	}
	dc.NegotiateAPIVersion(ctx)
	if err := dockerPullContainers(ctx, dc); err != nil {
		return fmt.Errorf("failed to pull all containers: %v", err)
	}
	if err := setupKNE(ctx); err != nil {
		return err
	}
	return setupTopology()
}

func validateDeps() error {
	var errs *multierror.Error
	if _, err := exec.LookPath("go"); err != nil {
		errs = multierror.Append(errs, err)
	}
	if _, err := exec.LookPath("kind"); err != nil {
		errs = multierror.Append(errs, err)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		errs = multierror.Append(errs, err)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		errs = multierror.Append(errs, err)
	}
	return errs.ErrorOrNil()
}

func cloneRepos(dir string) error {
	kneDir = filepath.Join(dir, "kne")
	if _, err := git.PlainClone(kneDir, false, &git.CloneOptions{URL: kneRepo}); err != nil {
		return fmt.Errorf("unable to clone kne github repo: %v", err)
	}
	kneRef, err := execCmdOutputDir(kneDir, "git", "show", "--oneline", "-s")
	if err != nil {
		return err
	}
	log.Infof("Cloned kne github repo: %v", kneRef)
	meshnetDir = filepath.Join(dir, "meshnet")
	if _, err := git.PlainClone(meshnetDir, false, &git.CloneOptions{URL: meshnetRepo}); err != nil {
		return fmt.Errorf("unable to clone meshnet github repo: %v", err)
	}
	meshnetRef, err := execCmdOutputDir(meshnetDir, "git", "show", "--oneline", "-s")
	if err != nil {
		return err
	}
	log.Infof("Cloned meshnet github repo: %v", meshnetRef)
	keysightDir = filepath.Join(dir, "keysight")
	if err := execCmd("gcloud", "source", "repos", "clone", "keysight", keysightDir, "--project=gep-kne"); err != nil {
		return fmt.Errorf("unable to clone keysight cloud source repo: %v", err)
	}
	keysightRef, err := execCmdOutputDir(keysightDir, "git", "show", "--oneline", "-s")
	if err != nil {
		return err
	}
	log.Infof("Cloned keysight cloud source repo: %v", keysightRef)
	return nil
}

func dockerName(container, tag string) (string, error) {
	fullName, ok := containers[container]
	if !ok {
		for _, v := range containers {
			if v == container {
				fullName = container
				break
			}
			return "", fmt.Errorf("container with name %q not recognized", container)
		}
	}
	if tag == "" {
		return fullName, nil
	}
	return fmt.Sprintf("%s:%s", fullName, tag), nil
}

func dockerPull(ctx context.Context, dc *dclient.Client, container, tag string) error {
	name, err := dockerName(container, tag)
	if err != nil {
		return err
	}
	log.Infof("Pulling %q", name)
	r, err := dc.ImagePull(ctx, name, types.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull container %q: %v", name, err)
	}
	io.Copy(os.Stdout, r)
	testName := fmt.Sprintf("%s:%s", container, testTag)
	if err := execCmd("docker", "tag", name, testName); err != nil {
		return err
	}
	return nil
}

func dockerPullGCP(container, tag string) error {
	name, err := dockerName(container, tag)
	if err != nil {
		return err
	}
	log.Infof("Pulling %q", name)
	// TODO: this isn't failing when the cmd actually fails
	if err := execCmd("docker", "pull", name); err != nil {
		return err
	}
	testName := fmt.Sprintf("%s:%s", container, testTag)
	if err := execCmd("docker", "tag", name, testName); err != nil {
		return err
	}
	log.Infof("Tagged container %q as %q", name, testName)
	return nil
}

func dockerPullContainers(ctx context.Context, dc *dclient.Client) error {
	var errs *multierror.Error
	multierror.Append(errs, dockerPull(ctx, dc, "meshnet", ""))
	multierror.Append(errs, dockerPull(ctx, dc, "init-wait", ""))
	multierror.Append(errs, dockerPull(ctx, dc, "alpine", ""))
	multierror.Append(errs, dockerPullGCP("ceos", *ceosVer))
	multierror.Append(errs, dockerPullGCP("athena-controller", *athenaControllerVer))
	multierror.Append(errs, dockerPullGCP("athena-engine", *athenaEngineVer))
	multierror.Append(errs, dockerPullGCP("athena-protocols", *athenaProtocolsVer))
	multierror.Append(errs, dockerPullGCP("athena-operator", *athenaOperatorVer))
	multierror.Append(errs, dockerPullGCP("athena-server", *athenaServerVer))
	return errs.ErrorOrNil()
}

func setupKNE(ctx context.Context) error {
	// Create kind cluster and wait up to 5 minutes for pods to be created.
	if err := execCmd("kind", "create", "cluster", "--name=kne", "--wait=5m"); err != nil {
		return err
	}
	if err := waitForPods(); err != nil {
		return err
	}
	if err := loadKNEContainers(); err != nil {
		return err
	}
	if err := execCmd("kubectl", "apply", "-k", filepath.Join(meshnetDir, "manifests", "base")); err != nil {
		return err
	}
	if err := waitForPods(); err != nil {
		return err
	}
	return setupLB(ctx)
}

func setupLB(ctx context.Context) error {
	if err := execCmd("kubectl", "apply", "-f", lbNamespaceURL); err != nil {
		return err
	}
	if err := waitForPods(); err != nil {
		return err
	}
	b := make([]byte, 128)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("failed to generate random bytes: %v", err)
	}
	k := base64.StdEncoding.EncodeToString(b)
	secretFlag := fmt.Sprintf("--from-literal=secretkey=%q", k)
	if err := execCmd("kubectl", "create", "secret", "generic", "-n", "metallb-system", "memberlist", secretFlag); err != nil {
		return err
	}
	if err := waitForPods(); err != nil {
		return err
	}
	cli, err := dclient.NewClientWithOpts(dclient.FromEnv)
	if err != nil {
		return err
	}
	cli.NegotiateAPIVersion(ctx)
	nr, err := cli.NetworkList(ctx, types.NetworkListOptions{Filters: filters.NewArgs(filters.Arg("name", "kind"))})
	if err != nil {
		return err
	}
	subnet := ""
	for _, cfg := range nr[0].IPAM.Config {
		if cfg.Gateway != "" {
			subnet = cfg.Subnet
		}
	}
	i := strings.LastIndex(subnet, ".")
	if i == -1 {
		return fmt.Errorf("subnet %q malformed", subnet)
	}
	addrPrefix := subnet[:i]
	configMap := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  namespace: metallb-system
  name: config
data:
  config: |
   address-pools:
    - name: default
      protocol: layer2
      addresses:
      - %s.100 - %s.250
`, addrPrefix, addrPrefix)
	fn := filepath.Join(testTmpDir, "metallb.yaml")
	if err := os.WriteFile(fn, []byte(configMap), 0644); err != nil {
		return fmt.Errorf("failed to write config map to file %q: %v", fn, err)
	}
	if err := execCmd("kubectl", "apply", "-f", fn); err != nil {
		return err
	}
	if err := startLBWithRetry(); err != nil {
		return err
	}
	return waitForPods()
}

func startLBWithRetry() error {
	attempt := uint64(1)
	max := uint64(10)
	if err := backoff.RetryNotify(startLB, backoff.WithMaxRetries(backoff.NewConstantBackOff(time.Second*30), max), func(err error, d time.Duration) {
		log.Warningf("Attempt [%v/%v] failed to setup metallb, trying again in %v: %v", attempt, max, d, err)
		attempt++
	}); err != nil {
		return err
	}
	return nil
}

func startLB() error {
	if err := execCmd("kubectl", "apply", "-f", lbURL); err != nil {
		return backoff.Permanent(err)
	}
	log.Info("Sleeping for 30s before checking metallb health...")
	time.Sleep(time.Second * 30)

	max := 5
	for i := 0; i < max; i++ {
		time.Sleep(time.Second)
		if err := checkLB(); err != nil {
			if err := execCmd("kubectl", "delete", "-f", lbURL); err != nil {
				return backoff.Permanent(err)
			}
			return err
		}
		log.Infof("metallb passed health check [%v/%v]", i+1, max)
	}
	return nil
}

func checkLB() error {
	out, err := execCmdOutput("kubectl", "get", "pods", "-l", "app=metallb", "-n", "metallb-system", "-o", `jsonpath={..status.conditions[?(@.type=="Ready")].status}`)
	if err != nil {
		return err
	}
	if strings.Contains(out, "False") {
		return fmt.Errorf("metallb pod not ready")
	}
	return nil
}

func loadKNEContainers() error {
	if err := execCmd("kind", "load", "docker-image", "meshnet:test", "--name=kne"); err != nil {
		return err
	}
	if err := execCmd("kind", "load", "docker-image", "init-wait:test", "--name=kne"); err != nil {
		return err
	}
	if err := execCmd("kind", "load", "docker-image", "alpine:test", "--name=kne"); err != nil {
		return err
	}
	return nil
}

func loadNodeContainers() error {
	if err := execCmd("kind", "load", "docker-image", "ceos:test", "--name=kne"); err != nil {
		return err
	}
	if err := execCmd("kind", "load", "docker-image", "athena-operator:test", "--name=kne"); err != nil {
		return err
	}
	if err := execCmd("kind", "load", "docker-image", "athena-server:test", "--name=kne"); err != nil {
		return err
	}
	if err := execCmd("kind", "load", "docker-image", "athena-controller:test", "--name=kne"); err != nil {
		return err
	}
	if err := execCmd("kind", "load", "docker-image", "athena-engine:test", "--name=kne"); err != nil {
		return err
	}
	if err := execCmd("kind", "load", "docker-image", "athena-protocols:test", "--name=kne"); err != nil {
		return err
	}
	return nil
}

func setupTopology() error {
	if err := loadNodeContainers(); err != nil {
		return err
	}
	if err := setupAthena(); err != nil {
		return err
	}
	if err := execCmd(kneCLIPath, "create", "cfg/topo.pb.txt"); err != nil {
		return err
	}
	return waitForPods()
}

func setupAthena() error {
	if err := execCmd("kubectl", "apply", "-f", filepath.Join(keysightDir, "v2", "athena", "controller", "athena.yaml")); err != nil {
		return err
	}
	if err := execCmd("kubectl", "patch", "service", "athena-service", "-p", `{"spec": {"type": "LoadBalancer"}}`); err != nil {
		return err
	}
	if err := execCmd("kubectl", "patch", "service", "gnmi-service", "-p", `{"spec": {"type": "LoadBalancer"}}`); err != nil {
		return err
	}
	if err := execCmd("kubectl", "apply", "-f", filepath.Join(keysightDir, "v2", "athena", "operator", "ixiatg-operator.yaml")); err != nil {
		return err
	}
	time.Sleep(5 * time.Second)
	if err := execCmd(
		"kubectl",
		"patch",
		"deployment",
		"ixiatg-op-controller-manager",
		"-n",
		"ixiatg-op-system",
		"--type=json",
		`-p=[{"op": "replace", "path": "/spec/template/spec/containers/1/image", "value":"athena-operator:test"}]`,
	); err != nil {
		return err
	}
	time.Sleep(time.Second * 15)
	return waitForDeployments()
}

func waitForPods() error {
	if err := execCmd("kubectl", "wait", "--for=condition=Ready", "pods", "--all", "-A", "--timeout=1m"); err != nil {
		log.Info("Current pod status:")
		execCmd("kubectl", "get", "pods", "-A")
		return err
	}
	return nil
}

func waitForDeployments() error {
	if err := execCmd("kubectl", "wait", "--for=condition=Available", "deployments", "--all", "-A", "--timeout=1m"); err != nil {
		log.Info("Current deployment status:")
		execCmd("kubectl", "get", "deployments", "-A")
		return err
	}
	return nil
}

func teardown() {
	fmt.Println("teardown")
	if err := os.RemoveAll(baseTmpDir); err != nil {
		log.Errorf("Failed to clean up temporary test dir %q: %v", baseTmpDir, err)
	}
	if err := execCmd("kind", "delete", "cluster", "--name=kne"); err != nil {
		log.Errorf("Failed to tear down kind cluster: %v", err)
	}
}

type nodeStr struct {
	name        string
	image       string
	constraints map[string]string
	reference   string
}

type link struct {
	node nodeStr
	peer nodeStr
}

type portInfo struct {
	tx uint64
	rx uint64
}

var (
	portMap = make(map[string]*portInfo)
)

func TestAthena(t *testing.T) {
	ctx := context.Background()
	controller, err := execCmdOutput("kubectl", "get", "services", "athena-service", "-o", `jsonpath="{.status.loadBalancer.ingress[0].ip}"`)
	if err != nil {
		t.Fatal(err)
	}
	controllerURL := fmt.Sprintf("https://%s:443", strings.Trim(controller, "\""))
	gnmi, err := execCmdOutput("kubectl", "get", "services", "gnmi-service", "-o", `jsonpath="{.status.loadBalancer.ingress[0].ip}"`)
	if err != nil {
		t.Fatal(err)
	}
	gnmiServer := fmt.Sprintf("%s:50051", strings.Trim(gnmi, "\""))
	ate1, err := execCmdOutput("kubectl", "get", "services", "service-ate1", "-n", "3node-traffic", "-o", `jsonpath="{.status.loadBalancer.ingress[0].ip}"`)
	if err != nil {
		t.Fatal(err)
	}
	ate1 = strings.Trim(ate1, "\"")
	ate2, err := execCmdOutput("kubectl", "get", "services", "service-ate2", "-n", "3node-traffic", "-o", `jsonpath="{.status.loadBalancer.ingress[0].ip}"`)
	if err != nil {
		t.Fatal(err)
	}
	ate2 = strings.Trim(ate2, "\"")

	r1JSON, err := execCmdOutput("kubectl", "exec", "r1", "-n", "3node-traffic", "-q", "--", "Cli", "-c", "show int e9|json")
	if err != nil {
		t.Fatal(err)
	}
	r1Addr, err := parseAddr(r1JSON)
	if err != nil {
		t.Fatal(err)
	}
	r3JSON, err := execCmdOutput("kubectl", "exec", "r3", "-n", "3node-traffic", "-q", "--", "Cli", "-c", "show int e9|json")
	if err != nil {
		t.Fatal(err)
	}
	r3Addr, err := parseAddr(r3JSON)
	if err != nil {
		t.Fatal(err)
	}

	trafficEngine1 := nodeStr{
		name:        "ate1",
		image:       "athena-engine:test", // "athena/traffic-engine:1.2.0.15",
		constraints: make(map[string]string),
		reference:   fmt.Sprintf(ateMapTemplate, ate1, ate1),
	}
	trafficEngine2 := nodeStr{
		name:        "ate2",
		image:       "athena-engine:test", // "athena/traffic-engine:1.2.0.15",
		constraints: make(map[string]string),
		reference:   fmt.Sprintf(ateMapTemplate, ate2, ate2),
	}
	fwdNode1 := nodeStr{
		name:        "r1",
		image:       "ceos:test", // "ceosimage:4.26.0F",
		constraints: make(map[string]string),
		reference:   "",
	}
	fwdNode2 := nodeStr{
		name:        "r3",
		image:       "ceos:test", // "ceosimage:4.26.0F",
		constraints: make(map[string]string),
		reference:   "",
	}

	configMap := []link{{
		node: trafficEngine1,
		peer: fwdNode1,
	}, {
		node: trafficEngine2,
		peer: fwdNode2,
	}}

	txPortName := "p1"
	rxPortName := "p2"
	flowName := "f1"

	client, err := otgclient.NewClientWithResponses(controllerURL, otgclient.WithHTTPClient(&http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}))
	if err != nil {
		log.Infof("Could not connect to %v", controllerURL)
		t.Fatalf("failed to get otgclient: %v", err)
	}
	log.Infof("Connected to controller at %v", controllerURL)

	macAddr1 := "00:15:00:00:00:01"
	macAddr2 := "00:16:00:00:00:01"
	mtu := 1500

	ipAddress1 := "20.20.20.1"
	ipGateway1 := "20.20.20.2"
	ipAddress2 := "30.30.30.1"
	ipGateway2 := "30.30.30.2"
	ipPrefix := 24

	bgpLocalAddress1 := "20.20.20.1"
	bgpDutAddress1 := "20.20.20.2"
	bgpRouterID1 := "20.20.20.1"
	bgpLocalAddress2 := "30.30.30.1"
	bgpDutAddress2 := "30.30.30.2"
	bgpRouterID2 := "30.30.30.1"
	bgpActive := true
	bgpAsType := "ebgp"
	bgpAsNumber := 3001
	nextHopAddress1 := "20.20.20.1"
	routeAddress1 := "11.11.11.1"
	nextHopAddress2 := "30.30.30.1"
	routeAddress2 := "12.12.12.1"
	routeCount := "2"
	routeStep := "0.0.1.0"
	routePrefix := 24

	// Test config setup
	durationType := "fixed_packets"
	rateType := "pps"
	packets := 10000
	pps := 1000
	srcMac1 := macAddr1
	srcMac2 := macAddr2
	dstMac1 := r1Addr
	dstMac2 := r3Addr
	dstIP := routeAddress2
	srcIP := routeAddress1

	ethDst1 := otgclient.PatternFlowEthernetDst{Choice: "value", Value: &dstMac1}
	ethSrc1 := otgclient.PatternFlowEthernetSrc{Choice: "value", Value: &srcMac1}
	pktEthernet1 := otgclient.FlowEthernet{Dst: &ethDst1, Src: &ethSrc1}
	ipDst1 := otgclient.PatternFlowIpv4Dst{Choice: "value", Value: &dstIP}
	ipSrc1 := otgclient.PatternFlowIpv4Src{Choice: "value", Value: &srcIP}
	pktIPV41 := otgclient.FlowIpv4{Dst: &ipDst1, Src: &ipSrc1}

	ethDst2 := otgclient.PatternFlowEthernetDst{Choice: "value", Value: &dstMac2}
	ethSrc2 := otgclient.PatternFlowEthernetSrc{Choice: "value", Value: &srcMac2}
	pktEthernet2 := otgclient.FlowEthernet{Dst: &ethDst2, Src: &ethSrc2}
	ipDst2 := otgclient.PatternFlowIpv4Dst{Choice: "value", Value: &srcIP}
	ipSrc2 := otgclient.PatternFlowIpv4Src{Choice: "value", Value: &dstIP}
	pktIPV42 := otgclient.FlowIpv4{Dst: &ipDst2, Src: &ipSrc2}

	resp, err := client.SetConfigWithResponse(ctx, otgclient.SetConfigJSONRequestBody{
		Flows: &[]otgclient.Flow{{
			Duration: &otgclient.FlowDuration{
				Choice: durationType,
				FixedPackets: &otgclient.FlowFixedPackets{
					Packets: &packets,
				},
			},
			Rate: &otgclient.FlowRate{
				Choice: rateType,
				Pps:    &pps,
			},
			Name: "f1",
			Packet: &[]otgclient.FlowHeader{
				otgclient.FlowHeader{
					Choice:   "ethernet",
					Ethernet: &pktEthernet1,
				},
				otgclient.FlowHeader{
					Choice: "ipv4",
					Ipv4:   &pktIPV41,
				},
			},
			TxRx: otgclient.FlowTxRx{
				Choice: "port",
				Port: &otgclient.FlowPort{
					TxName: txPortName,
					RxName: &rxPortName,
				},
			},
		}, {
			Duration: &otgclient.FlowDuration{
				Choice: durationType,
				FixedPackets: &otgclient.FlowFixedPackets{
					Packets: &packets,
				},
			},
			Rate: &otgclient.FlowRate{
				Choice: rateType,
				Pps:    &pps,
			},
			Name: "f2",
			Packet: &[]otgclient.FlowHeader{
				otgclient.FlowHeader{
					Choice:   "ethernet",
					Ethernet: &pktEthernet2,
				},
				otgclient.FlowHeader{
					Choice: "ipv4",
					Ipv4:   &pktIPV42,
				},
			},
			TxRx: otgclient.FlowTxRx{
				Choice: "port",
				Port: &otgclient.FlowPort{
					TxName: rxPortName,
					RxName: &txPortName,
				},
			},
		}},
		Ports: &[]otgclient.Port{
			otgclient.Port{
				Location: &configMap[0].node.reference,
				Name:     txPortName,
			},
			otgclient.Port{
				Location: &configMap[1].node.reference,
				Name:     rxPortName,
			},
		},
		Devices: &[]otgclient.Device{
			{
				Name:          "DeviceGroup1",
				ContainerName: txPortName,
				Ethernet: otgclient.DeviceEthernet{
					Name: "Ethernet1",
					Mac:  &macAddr1,
					Mtu:  &mtu,
					Ipv4: &otgclient.DeviceIpv4{
						Name:    "IPv41",
						Address: &ipAddress1,
						Gateway: &ipGateway1,
						Prefix:  &ipPrefix,
						Bgpv4: &otgclient.DeviceBgpv4{
							Name:         "BGP Peer 1",
							LocalAddress: &bgpLocalAddress1,
							DutAddress:   &bgpDutAddress1,
							RouterId:     &bgpRouterID1,
							Active:       &bgpActive,
							AsType:       &bgpAsType,
							AsNumber:     &bgpAsNumber,
							Bgpv4Routes: &[]otgclient.DeviceBgpv4Route{
								{
									Name:           "RR 1",
									NextHopAddress: &nextHopAddress1,
									Addresses: &[]otgclient.DeviceBgpv4RouteAddress{
										{
											Address: &routeAddress1,
											Count:   &routeCount,
											Step:    &routeStep,
											Prefix:  &routePrefix,
										},
									},
								},
							},
						},
					},
				},
			},
			{
				Name:          "DeviceGroup2",
				ContainerName: rxPortName,
				Ethernet: otgclient.DeviceEthernet{
					Name: "Ethernet1",
					Mac:  &macAddr2,
					Mtu:  &mtu,
					Ipv4: &otgclient.DeviceIpv4{
						Name:    "IPv41",
						Address: &ipAddress2,
						Gateway: &ipGateway2,
						Prefix:  &ipPrefix,
						Bgpv4: &otgclient.DeviceBgpv4{
							Name:         "BGP Peer 1",
							LocalAddress: &bgpLocalAddress2,
							DutAddress:   &bgpDutAddress2,
							RouterId:     &bgpRouterID2,
							Active:       &bgpActive,
							AsType:       &bgpAsType,
							AsNumber:     &bgpAsNumber,
							Bgpv4Routes: &[]otgclient.DeviceBgpv4Route{
								{
									Name:           "RR 1",
									NextHopAddress: &nextHopAddress2,
									Addresses: &[]otgclient.DeviceBgpv4RouteAddress{
										{
											Address: &routeAddress2,
											Count:   &routeCount,
											Step:    &routeStep,
											Prefix:  &routePrefix,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	})

	if hasErrorInSetConfigResponse(t, resp, err) {
		t.Fatalf("failed to set config: %v", err)
	}

	var respData interface{}
	err = json.Unmarshal(resp.Body, &respData)
	if err != nil {
		t.Fatalf("failed to parse set config response: %v", err)
	}

	dict, _ := respData.(map[string]interface{})
	if hasError(t, dict["warnings"]) || hasError(t, dict["errors"]) {
		return
	}

	log.Info("Ixia ports have been configured, waiting 10 seconds before setting up traffic...")
	time.Sleep(10 * time.Second)

	transResp, err := client.SetTransmitStateWithResponse(ctx, otgclient.SetTransmitStateJSONRequestBody{
		State:     "start",
		FlowNames: &[]string{"f1", "f2"},
	})

	if hasErrorInSetTransmitResponse(t, transResp, err) {
		t.Fatalf("failed to start traffic: %v", err)
	}

	log.Infof("PacketForward config is set and traffic started, verifying stats...")

	iter := 0
	maxIter := 10
	tol := 1
	for iter < maxIter {
		log.Infof("[%v/%v] Polling stats...", iter+1, maxIter)
		if err = executePoll(ctx, gnmiServer, txPortName, rxPortName, flowName); err != nil {
			t.Fatalf("failed to get executePoll: %v", err)
		}
		if withinTolerance(portMap[txPortName].tx, uint64(packets), uint64(tol)) && withinTolerance(portMap[flowName].rx, uint64(packets), uint64(tol)) {
			log.Infof("\tNumber of Tx and Rx packets match - final stats:\n")
			for k, v := range portMap {
				log.Infof("\t\tName: %s, Tx: %v, Rx: %v\n", k, v.tx, v.rx)
			}
			_, err = client.SetConfigWithResponse(ctx, otgclient.SetConfigJSONRequestBody{})
			if err != nil {
				t.Errorf("failed to clear config: %v", err)
			}
			return
		}
		log.Info("\tPort stats:")
		for k, v := range portMap {
			log.Infof("\t\tName: %s, Tx: %v, Rx: %v", k, v.tx, v.rx)
		}
		dumpFlowStats(ctx, client)
		if portMap[txPortName].tx > uint64(packets) || portMap[flowName].rx > uint64(packets) {
			break
		}
		iter++
	}

	t.Errorf("failed to get correct stats - final stats:\n")
	for k, v := range portMap {
		t.Errorf("%s: %+v\n", k, *v)
	}
}

func withinTolerance(got, want, epsilon uint64) bool {
	return want-epsilon <= got && got <= want+epsilon
}

func dumpFlowStats(ctx context.Context, c *otgclient.ClientWithResponses) {
	resp, err := c.GetMetricsWithResponse(ctx, otgclient.GetMetricsJSONRequestBody{
		Choice: "flow",
		Flow: &otgclient.FlowMetricsRequest{
			FlowNames: &[]string{"f1", "f2"},
		},
	})
	if err != nil {
		log.Errorf("failed to get controller metrics")
	} else {
		log.Info("\tController stats:")
		for _, v := range *resp.JSON200.FlowMetrics {
			log.Infof("\t\tName: %s, Tx: %v, Rx: %v", *v.Name, *v.FramesTx, *v.FramesRx)
		}
	}
}

func parseAddr(jsonStr string) (string, error) {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &m); err != nil {
		return "", err
	}
	addr := m["interfaces"].(map[string]interface{})["Ethernet9"].(map[string]interface{})["physicalAddress"].(string)
	return strings.Trim(addr, "\""), nil
}

// TODO: either fix or remove
//func macAddr(ctx context.Context, addr string) (string, error) {
//	mac := ""
//
//	q := client.Query{TLS: &tls.Config{}}
//	q.Addrs = []string{addr}
//	q.Timeout = 10 * time.Second
//	q.Credentials = nil
//	q.TLS = nil
//	q.Queries = append(q.Queries, []string{"/interfaces/interface[name=Ethernet9]/ethernet/state/mac-address"})
//	q.Type = client.Once
//	q.ProtoHandler = func(msg proto.Message) error {
//		v := msg.(*gpb.SubscribeResponse)
//		notification := v.GetUpdate()
//		updates := notification.GetUpdate()
//		for _, update := range updates {
//			var data interface{}
//			err := json.Unmarshal(update.Val.GetJsonVal(), &data)
//			if err != nil {
//				return nil
//			}
//			log.Infof("Received proto for mac addr: %v", data)
//		}
//		return nil
//	}
//
//	c := &client.BaseClient{}
//	clientTypes := []string{}
//	if err := c.Subscribe(ctx, q, clientTypes...); err != nil {
//		return "", fmt.Errorf("client had error while displaying results:\n\t%v", err)
//	}
//	return mac, nil
//}

func executePoll(ctx context.Context, gnmiServer, txPortName, rxPortName, flowName string) error {
	portMap[txPortName] = &portInfo{tx: 0, rx: 0}
	portMap[rxPortName] = &portInfo{tx: 0, rx: 0}
	portMap[flowName] = &portInfo{tx: 0, rx: 0}

	q := client.Query{TLS: &tls.Config{}}
	q.Addrs = []string{gnmiServer}
	q.Timeout = 10 * time.Second
	q.Credentials = nil
	q.TLS = nil
	q.Queries = append(q.Queries, []string{"port_metrics[name=" + txPortName + "]"})
	q.Queries = append(q.Queries, []string{"port_metrics[name=" + rxPortName + "]"})
	q.Queries = append(q.Queries, []string{"flow_metrics[name=" + flowName + "]"})
	q.Type = client.Once
	q.ProtoHandler = func(msg proto.Message) error {
		v := msg.(*gpb.SubscribeResponse)
		notification := v.GetUpdate()
		updates := notification.GetUpdate()
		for _, update := range updates {
			var data interface{}
			err := json.Unmarshal(update.Val.GetJsonVal(), &data)
			if err != nil {
				return nil
			}
			dict, _ := data.(map[string]interface{})
			portName, _ := dict["name"].(string)
			portTx, _ := dict["frames_tx"].(float64)
			portRx, _ := dict["frames_rx"].(float64)
			if _, ok := portMap[portName]; ok {
				portMap[portName].tx = uint64(portTx)
				portMap[portName].rx = uint64(portRx)
			}
		}
		return nil
	}

	c := &client.BaseClient{}
	clientTypes := []string{}
	if err := c.Subscribe(ctx, q, clientTypes...); err != nil {
		return fmt.Errorf("client had error while displaying results:\n\t%v", err)
	}
	return nil
}

func dumpProtocolStats(ctx context.Context, c *otgclient.ClientWithResponses, protocol string) {
	resp, err := c.GetMetricsWithResponse(ctx, otgclient.GetMetricsJSONRequestBody{
		Choice: protocol,
		Bgpv4: &otgclient.Bgpv4MetricsRequest{
			DeviceNames: &[]string{},
		},
	})

	if err != nil {
		log.Errorf("failed to get controller metrics\n")
		return
	}
	log.Infof("%s stats: %v: \n", protocol, resp.JSON200)
	if resp.JSON200 == nil {
		log.Infof("Cannot fetch protocols stats, protocols stat is nill: %v: \n", resp.JSON200)
	}
	for _, v := range *resp.JSON200.Bgpv4Metrics {
		log.Infof("Controller Stats: Name: %s: \n", *v.Name)
		log.Infof("\tSessionsTotal: %v: \n", *v.SessionsTotal)
		log.Infof("\tSessionsUp: %v: \n", *v.SessionsUp)
		log.Infof("\tSessionsDown: %v: \n", *v.SessionsDown)
		log.Infof("\tSessionsNotStarted: %v: \n", *v.SessionsNotStarted)
		log.Infof("\tRoutesAdvertised: %v: \n", *v.RoutesAdvertised)
		log.Infof("\tRoutesWithdrawn: %v: \n", *v.RoutesWithdrawn)
	}
}

func hasErrorInSetConfigResponse(t *testing.T, resp *otgclient.SetConfigResponse, err error) bool {
	if err != nil {
		log.Errorf("SetConfig returned error : %v", err)
		return true
	}
	var respData interface{}
	err = json.Unmarshal(resp.Body, &respData)
	if err != nil {
		log.Errorf("failed to parse SetConfig response: %v", err)
		return true
	}
	dict, _ := respData.(map[string]interface{})
	if hasError(t, dict["warnings"]) || hasError(t, dict["errors"]) {
		return true
	}
	return false
}

func hasErrorInSetTransmitResponse(t *testing.T, resp *otgclient.SetTransmitStateResponse, err error) bool {
	if err != nil {
		log.Errorf("Failed to start traffic: %v", err)
		return true
	}
	var respData interface{}
	err = json.Unmarshal(resp.Body, &respData)
	if err != nil {
		log.Errorf("failed to parse SetTransmit response: %v", err)
		return true
	}
	dict, _ := respData.(map[string]interface{})
	if hasError(t, dict["warnings"]) || hasError(t, dict["errors"]) {
		return true
	}
	return false
}

func hasError(t *testing.T, value interface{}) bool {
	if value != nil {
		e := value.([]interface{})
		if len(e) > 0 {
			t.Errorf("Error in SetTransmit response: %+v", e)
			return true
		}
	}
	return false
}

func execCmd(cmd string, args ...string) error {
	c := exec.Command(cmd, args...)
	c.Stderr = os.Stderr
	c.Stdout = os.Stdout
	log.Infof("`%s`", c.String())
	if err := c.Run(); err != nil {
		return fmt.Errorf("`%s` failed: %v", c.String(), err)
	}
	return nil
}

func execCmdDir(dir string, cmd string, args ...string) error {
	c := exec.Command(cmd, args...)
	c.Dir = dir
	c.Stderr = os.Stderr
	c.Stdout = os.Stdout
	log.Infof("`%s`", c.String())
	if err := c.Run(); err != nil {
		return fmt.Errorf("`%s` failed: %v", c.String(), err)
	}
	return nil
}

func execCmdOutput(cmd string, args ...string) (string, error) {
	c := exec.Command(cmd, args...)
	log.Infof("`%s`", c.String())
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("`%s` failed: %v: %v", c.String(), err, string(out))
	}
	return string(out), nil
}

func execCmdOutputDir(dir string, cmd string, args ...string) (string, error) {
	c := exec.Command(cmd, args...)
	c.Dir = dir
	log.Infof("`%s`", c.String())
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("`%s` failed: %v: %v", c.String(), err, string(out))
	}
	return string(out), nil
}
