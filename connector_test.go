package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"go.arcalot.io/assert"
	log "go.arcalot.io/log/v2"
	"go.flow.arcalot.io/deployer"
	"go.flow.arcalot.io/podmandeployer/tests"
)

func getConnector(t *testing.T, configJSON string) (deployer.Connector, *Config) {
	var config any
	err := json.Unmarshal([]byte(configJSON), &config)
	assert.NoError(t, err)
	factory := NewFactory()
	schema := factory.ConfigurationSchema()
	unserializedConfig, err := schema.UnserializeType(config)
	assert.NoError(t, err)
	connector, err := factory.Create(unserializedConfig, log.NewTestLogger(t))
	assert.NoError(t, err)
	return connector, unserializedConfig
}

var inOutConfig = `
{
   "podman":{
      "path":"/usr/bin/podman"
   }
}
`

func TestSimpleInOut(t *testing.T) {
	logger := log.NewTestLogger(t)
	pongStr := "pong abc"
	endStr := "end abc"

	connector, _ := getConnector(t, inOutConfig)
	plugin, err := connector.Deploy(
		context.Background(),
		"quay.io/tsebastiani/arcaflow-engine-deployer-podman-test:latest")
	assert.NoError(t, err)

	var containerInput = []byte("ping abc\n")
	assert.NoErrorR[int](t)(plugin.Write(containerInput))

	readBuffer := readOutputUntil(t, plugin, pongStr)
	// assert output is not empty
	assert.Equals(t, len(readBuffer) > 0, true)

	logger.Infof(string(readBuffer[:7]))
	assert.NoErrorR[int](t)(plugin.Write(containerInput))

	readBuffer = readOutputUntil(t, plugin, endStr)
	// assert output is not empty
	assert.Equals(t, len(readBuffer) > 0, true)

	t.Cleanup(func() {
		assert.NoError(t, plugin.Close())
	})
}

var envConfig = `
{
   "deployment":{
      "container":{
         "NetworkDisabled":true,
         "Env":[
            "DEPLOYER_PODMAN_TEST_1=TEST1",
            "DEPLOYER_PODMAN_TEST_2=TEST2"
         ]
      }
   },
   "podman":{
      "path":"/usr/bin/podman"
   }
}
`

func TestEnv(t *testing.T) {
	envVars := "DEPLOYER_PODMAN_TEST_1=TEST1\nDEPLOYER_PODMAN_TEST_2=TEST2"
	connector, _ := getConnector(t, envConfig)
	container, err := connector.Deploy(context.Background(), "quay.io/tsebastiani/arcaflow-engine-deployer-podman-test:latest")
	assert.NoError(t, err)

	var containerInput = []byte("env\n")
	assert.NoErrorR[int](t)(container.Write(containerInput))

	readBuffer := readOutputUntil(t, container, envVars)
	assert.Equals(t, len(readBuffer) > 0, true)

	t.Cleanup(func() {
		assert.NoError(t, container.Close())
	})
}

var volumeConfig = `
{
   "deployment":{
      "host":{
         "Binds":[
            "./tests/volume:/test"
         ]
      }
   },
   "podman":{
      "path":"/usr/bin/podman"
   }
}
`

func TestSimpleVolume(t *testing.T) {
	logger := log.NewTestLogger(t)
	fileContent, err := os.ReadFile("./tests/volume/test_file.txt")
	assert.NoError(t, err)

	connector, _ := getConnector(t, volumeConfig)
	cwd, err := os.Getwd()
	assert.NoError(t, err)
	// disable selinux on the test folder in order to make the file readable from within the container
	cmd := exec.Command("chcon", "-Rt", "svirt_sandbox_file_t", fmt.Sprintf("%s/tests/volume", cwd)) //nolint:gosec
	err = cmd.Run()
	if err != nil {
		logger.Warningf("failed to set SELinux permissions on folder, chcon error: %s, this may cause test failure, let's see...", err.Error())
	}

	container, err := connector.Deploy(
		context.Background(),
		"quay.io/tsebastiani/arcaflow-engine-deployer-podman-test:latest")
	assert.NoError(t, err)

	var containerInput = []byte("volume\n")
	_, err = container.Write(containerInput)
	assert.NoError(t, err)

	readBuffer := readOutputUntil(t, container, string(fileContent))
	assert.Equals(t, len(readBuffer) > 0, true)

	t.Cleanup(func() {
		assert.NoError(t, container.Close())
	})
}

var nameTemplate = `
{
  "podman":{
     "path":"/usr/bin/podman",
     "containerNamePrefix":"%s"
  }
}
`

func TestContainerName(t *testing.T) {
	logger := log.NewTestLogger(t)
	configTemplate1 := fmt.Sprintf(nameTemplate, "test_1")
	configTemplate2 := fmt.Sprintf(nameTemplate, "test_2")

	ctx := context.Background()
	connector1, cfg1 := getConnector(t, configTemplate1)
	connector2, cfg2 := getConnector(t, configTemplate2)

	container1, err := connector1.Deploy(
		ctx,
		"quay.io/tsebastiani/arcaflow-engine-deployer-podman-test:latest")
	assert.NoError(t, err)

	container2, err := connector2.Deploy(
		ctx,
		"quay.io/tsebastiani/arcaflow-engine-deployer-podman-test:latest")
	assert.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var containerInput = []byte("sleep 3\n")
		assert.NoErrorR[int](t)(container1.Write(containerInput))
		assert.NoErrorR[int](t)(container2.Write(containerInput))
	}()

	time.Sleep(1 * time.Second)
	assert.Equals(t, tests.IsContainerRunning(logger, cfg1.Podman.Path, container1.ID()), true)

	time.Sleep(1 * time.Second)
	assert.Equals(t, tests.IsContainerRunning(logger, cfg2.Podman.Path, container2.ID()), true)

	wg.Wait()

	assert.Equals(t, container1.ID() != container2.ID(), true)

	t.Cleanup(func() {
		assert.NoError(t, container1.Close())
		assert.NoError(t, container2.Close())
	})
}

var cgroupTemplate = `
{
   "podman":{
      "path":"/usr/bin/podman",
      "containerNamePrefix":"%s",
      "cgroupNs":"%s"
   }
}
`

func TestCgroupNsByContainerName(t *testing.T) {
	if tests.IsRunningOnGithub() {
		t.Skipf("joining another container cgroup namespace by container name not supported on GitHub actions")
	}
	logger := log.NewTestLogger(t)

	containerNamePrefix1 := "test_1"
	// The first container will run with a private namespace that will be created at startup
	configtemplate1 := fmt.Sprintf(cgroupTemplate, containerNamePrefix1, "private")
	connector1, config := getConnector(t, configtemplate1)
	container1, err := connector1.Deploy(
		context.Background(),
		"quay.io/tsebastiani/arcaflow-engine-deployer-podman-test:latest")
	assert.NoError(t, err)

	containerNamePrefix2 := "test_2"
	// The second one will join the newly created private namespace of the first container
	configtemplate2 := fmt.Sprintf(cgroupTemplate, containerNamePrefix2, fmt.Sprintf("container:%s", container1.ID()))
	connector2, _ := getConnector(t, configtemplate2)
	container2, err := connector2.Deploy(
		context.Background(),
		"quay.io/tsebastiani/arcaflow-engine-deployer-podman-test:latest")
	assert.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var containerInput = []byte("sleep 7\n")
		_, err := container1.Write(containerInput)
		assert.NoError(t, err)
	}()
	// sleeps to wait the first container become ready and attach to its cgroup ns
	time.Sleep(3 * time.Second)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var containerInput = []byte("sleep 5\n")
		_, err := container2.Write(containerInput)
		assert.NoError(t, err)
	}()
	ns1 := tests.GetPodmanPsNsWithFormat(logger, config.Podman.Path, container1.ID(), "{{.CGROUPNS}}")
	ns2 := tests.GetPodmanPsNsWithFormat(logger, config.Podman.Path, container2.ID(), "{{.CGROUPNS}}")
	assert.Equals(t, ns1 == ns2, true)
	wg.Wait()

	t.Cleanup(func() {
		assert.NoError(t, container1.Close())
		assert.NoError(t, container2.Close())
	})
}

func TestPrivateCgroupNs(t *testing.T) {
	// get the user cgroup ns
	logger := log.NewTestLogger(t)

	var wg sync.WaitGroup
	userCgroupNs := tests.GetCommmandCgroupNs(logger, "/usr/bin/sleep", []string{"3"})
	assert.NotNil(t, userCgroupNs)
	logger.Debugf("Detected cgroup namespace for user: %s", userCgroupNs)

	containerNamePrefix := "test"
	// The first container will run with a private namespace that will be created at startup
	configtemplate := fmt.Sprintf(cgroupTemplate, containerNamePrefix, "private")
	connector, config := getConnector(t, configtemplate)
	container, err := connector.Deploy(
		context.Background(),
		"quay.io/tsebastiani/arcaflow-engine-deployer-podman-test:latest")
	assert.NoError(t, err)

	wg.Add(1)
	go func() {
		defer wg.Done()
		var containerInput = []byte("sleep 5\n")
		assert.NoErrorR[int](t)(container.Write(containerInput))
	}()

	time.Sleep(2 * time.Second)

	var podmanCgroupNs = tests.GetPodmanCgroupNs(logger, config.Podman.Path, container.ID())
	wg.Wait()

	assert.Equals(t, userCgroupNs != podmanCgroupNs, true)

	t.Cleanup(func() {
		assert.NoError(t, container.Close())
	})
}

func TestHostCgroupNs(t *testing.T) {
	logger := log.NewTestLogger(t)
	var wg sync.WaitGroup

	userCgroupNs := tests.GetCommmandCgroupNs(logger, "/usr/bin/sleep", []string{"3"})
	assert.NotNil(t, userCgroupNs)

	logger.Debugf("Detected cgroup namespace for user: %s", userCgroupNs)
	containerNamePrefix := "host_cgroupns"
	// The first container will run with the host namespace
	configtemplate := fmt.Sprintf(cgroupTemplate, containerNamePrefix, "host")
	connector, config := getConnector(t, configtemplate)
	container, err := connector.Deploy(
		context.Background(),
		"quay.io/tsebastiani/arcaflow-engine-deployer-podman-test:latest")
	assert.NoError(t, err)

	wg.Add(1)
	go func() {
		defer wg.Done()
		var containerInput = []byte("sleep 5\n")
		assert.NoErrorR[int](t)(container.Write(containerInput))
	}()
	wg.Wait()
	// waits for the container to become ready
	time.Sleep(2 * time.Second)

	var podmanCgroupNs = tests.GetPodmanCgroupNs(logger, config.Podman.Path, container.ID())
	assert.NotNil(t, podmanCgroupNs)
	wg.Wait()

	assert.Equals(t, userCgroupNs == podmanCgroupNs, true)

	t.Cleanup(func() {
		assert.NoError(t, container.Close())
	})
}

func TestCgroupNsByNamespacePath(t *testing.T) {
	if tests.IsRunningOnGithub() {
		t.Skipf("joining another container cgroup namespace by namespace path ns:/proc/<PID>/ns/cgroup not supported on GitHub actions")
	}
	logger := log.NewTestLogger(t)
	containerNamePrefix1 := "test_1"
	// The first container will run with a private namespace that will be created at startup
	configtemplate1 := fmt.Sprintf(cgroupTemplate, containerNamePrefix1, "private")
	connector1, config := getConnector(t, configtemplate1)
	container1, err := connector1.Deploy(context.Background(), "quay.io/tsebastiani/arcaflow-engine-deployer-podman-test:latest")
	assert.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var containerInput = []byte("sleep 10\n")
		_, err := container1.Write(containerInput)
		assert.NoError(t, err)
	}()
	// sleeps to wait the first container become ready and attach to its cgroup ns
	time.Sleep(2 * time.Second)

	pid := tests.GetPodmanPsNsWithFormat(logger, config.Podman.Path, container1.ID(), "{{.Pid}}")

	containerNamePrefix2 := "test_2"
	// The second one will join the newly created private namespace of the first container
	namespacePath := fmt.Sprintf("ns:/proc/%s/ns/cgroup", pid)
	configtemplate2 := fmt.Sprintf(cgroupTemplate, containerNamePrefix2, namespacePath)
	connector2, _ := getConnector(t, configtemplate2)

	container2, err := connector2.Deploy(
		context.Background(),
		"quay.io/tsebastiani/arcaflow-engine-deployer-podman-test:latest")
	assert.NoError(t, err)

	wg.Add(1)
	go func() {
		defer wg.Done()
		var containerInput = []byte("sleep 5\n")
		_, err := container2.Write(containerInput)
		assert.NoError(t, err)
	}()

	ns1 := tests.GetPodmanPsNsWithFormat(logger, config.Podman.Path, container1.ID(), "{{.CGROUPNS}}")
	ns2 := tests.GetPodmanPsNsWithFormat(logger, config.Podman.Path, container1.ID(), "{{.CGROUPNS}}")
	assert.Equals(t, ns1 == ns2, true)
	wg.Wait()

	t.Cleanup(func() {
		assert.NoError(t, container1.Close())
		assert.NoError(t, container2.Close())
	})
}

var networkTemplate = `
{
   "podman":{
      "containerNamePrefix":"%s",
      "path":"/usr/bin/podman",
      "networkMode":"%s"
   }
}
`

func TestNetworkHost(t *testing.T) {
	logger := log.NewTestLogger(t)
	containerNamePrefix := "networkhost"
	// The first container will run with the host namespace
	configtemplate := fmt.Sprintf(networkTemplate, containerNamePrefix, "host")
	connector, _ := getConnector(t, configtemplate)
	plugin, err := connector.Deploy(
		context.Background(),
		"quay.io/tsebastiani/arcaflow-engine-deployer-podman-test:latest")
	assert.NoError(t, err)

	var containerInput = []byte("network host\n")
	// the test script will run "ifconfig" in the container
	assert.NoErrorR[int](t)(plugin.Write(containerInput))

	var ifconfigOut bytes.Buffer
	// runs ifconfig in the host machine in order to check if the container has exactly the same network configuration
	cmd := exec.Command(
		"/bin/bash", "-c", "ifconfig | grep -P \"^.+:\\s+.+$\" | awk '{ gsub(\":\",\"\");print $1 }'")
	cmd.Stdout = &ifconfigOut
	assert.NoError(t, cmd.Run())

	ifconfigOutStr := ifconfigOut.String()
	logger.Infof(ifconfigOutStr)
	readBuffer := readOutputUntil(t, plugin, ifconfigOutStr)
	containerOutString := string(readBuffer)
	assert.Contains(t, containerOutString, ifconfigOutStr)

	t.Cleanup(func() {
		assert.NoError(t, plugin.Close())
	})
}

func TestNetworkBridge(t *testing.T) {
	// If this test breaks again, delete it.

	// This test forces the container to have the following
	// network settings:
	// ip 10.88.0.123
	// mac 44:33:22:11:00:99
	// then asks to the container to run an ifconfig (tests/test_script.sh, test_network())
	// through ATP to check if the settings have been effectively accepted
	if tests.IsRunningOnGithub() {
		t.Skipf("bridge networking not supported on GitHub actions")
	}
	ip := "10.88.0.123"
	mac := "44:33:22:11:00:99"

	testNetworking(
		t,
		"bridge:ip=10.88.0.123,mac=44:33:22:11:00:99,interface_name=testif0",
		"network bridge\n",
		nil,
		&ip,
		&mac,
	)
}
func TestNetworkNone(t *testing.T) {
	expectedOutput := "1;lo"
	testNetworking(t, "none", "network none\n", &expectedOutput, nil, nil)
}

func TestClose(t *testing.T) {
	containerNamePrefix := "close"
	configTemplate := fmt.Sprintf(nameTemplate, containerNamePrefix)
	connector, _ := getConnector(t, configTemplate)

	container, err := connector.Deploy(
		context.Background(),
		"quay.io/tsebastiani/arcaflow-engine-deployer-podman-test:latest")
	assert.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var containerInput = []byte("sleep 10\n")
		assert.NoErrorR[int](t)(container.Write(containerInput))
	}()

	time.Sleep(2 * time.Second)
	err = container.Close()
	assert.NoError(t, err)
}

// readOutputUntil helper function, reads from plugin (io.Reader) until finds lookforOutput
func readOutputUntil(t *testing.T, plugin io.Reader, lookForOutput string) []byte {
	var n int
	readBuffer := make([]byte, 10240)
	for {
		currentBuffer := make([]byte, 1024)
		readBytes, err := plugin.Read(currentBuffer)
		if err != nil {
			if err != io.EOF {
				t.Fatalf("error while reading stdout: %s", err.Error())
			} else {
				return readBuffer[:n]
			}
		}
		copy(readBuffer[n:], currentBuffer[:readBytes])
		n += readBytes
		if strings.Contains(string(readBuffer[:n]), lookForOutput) {
			return readBuffer[:n]
		}
	}
}

func checkIfconfig(t *testing.T) string {
	path, err := exec.LookPath("ifconfig")
	if err != nil {
		t.Fatalf("impossible to run test: %s , ifconfig not installed, skipping.", t.Name())
	}
	assert.NoError(t, err)
	return path
}

func testNetworking(t *testing.T, podmanNetworking string, containerTest string, expectedOutput *string, ip *string, mac *string) {
	logger := log.NewTestLogger(t)
	checkIfconfig(t)

	containerNamePrefix := "networking"
	// The first container will run with the host namespace
	configtemplate := fmt.Sprintf(networkTemplate, containerNamePrefix, podmanNetworking)
	connector, _ := getConnector(t, configtemplate)
	plugin, err := connector.Deploy(
		context.Background(),
		"quay.io/tsebastiani/arcaflow-engine-deployer-podman-test:latest")
	assert.NoError(t, err)

	var containerInput = []byte(containerTest)
	// the test script will output a string containing the desired ip address and mac address
	// filtered by the desired interface name
	_, err = plugin.Write(containerInput)
	assert.NoError(t, err)

	var readBuffer []byte
	if expectedOutput != nil {
		// in the networking none the token is exactly the output of ifconfig
		readBuffer = readOutputUntil(t, plugin, *expectedOutput)
	} else if mac != nil {
		// if an ip is passed instead the output contains the ipv6 interface ID as well so
		// the output is read until the mac address that is the last token in the ifconfig output.
		readBuffer = readOutputUntil(t, plugin, *mac)
	}
	logger.Infof(string(readBuffer))

	// assert the container input is not empty
	assert.Equals(t, len(readBuffer) > 0, true)

	if ip == nil && mac == nil && expectedOutput != nil {
		assert.Contains(t, string(readBuffer), *expectedOutput)
	} else {
		assert.Contains(t, string(readBuffer), *ip)
		assert.Contains(t, string(readBuffer), *mac)
	}

	t.Cleanup(func() {
		assert.NoError(t, plugin.Close())
	})
}
