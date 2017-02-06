package nginx

import (
	"testing"

	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"

	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/sky-uk/feed/controller"
	"github.com/sky-uk/feed/util/metrics"
	"github.com/stretchr/testify/assert"
)

func init() {
	metrics.SetConstLabels(make(prometheus.Labels))
}

const (
	port          = 9090
	fakeNginx     = "./fake_graceful_nginx.py"
	smallWaitTime = time.Millisecond * 20
)

func newConf(tmpDir string, binary string) Conf {
	return Conf{
		WorkingDir:                   tmpDir,
		BinaryLocation:               binary,
		IngressPort:                  port,
		WorkerProcesses:              1,
		BackendKeepalives:            1024,
		BackendConnectTimeoutSeconds: 1,
		ServerNamesHashMaxSize:       -1,
		ServerNamesHashBucketSize:    -1,
		UpdatePeriod:                 time.Second,
	}
}

func newUpdater(tmpDir string) controller.Updater {
	return newUpdaterWithBinary(tmpDir, fakeNginx)
}

func newUpdaterWithBinary(tmpDir string, binary string) controller.Updater {
	conf := newConf(tmpDir, binary)
	return newNginxWithConf(conf)
}

func newNginxWithConf(conf Conf) controller.Updater {
	lb := New(conf)
	return lb
}

func TestCanStartThenStop(t *testing.T) {
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	lb := newUpdater(tmpDir)

	assert.NoError(t, lb.Start())
	assert.NoError(t, lb.Stop())
}

func TestStopWaitsForGracefulShutdownOfNginx(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	lb := newUpdaterWithBinary(tmpDir, "./fake_graceful_nginx.py")

	assert.NoError(lb.Start())
	assert.NoError(lb.Stop())
	assert.Error(lb.Health(), "should have waited for nginx to gracefully stop")
}

func TestUnhealthyIfHealthPortIsNotUp(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	lb := newUpdater(tmpDir)

	assert.NoError(lb.Start())

	time.Sleep(smallWaitTime)
	assert.Error(lb.Health(), "should be unhealthy")
}

func TestUnhealthyUntilInitialUpdate(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	ts := stubHealthPort()
	defer ts.Close()
	conf := newConf(tmpDir, "./fake_graceful_nginx.py")
	conf.HealthPort = getPort(ts)
	lb := newNginxWithConf(conf)

	assert.EqualError(lb.Health(), "nginx is not running")
	assert.NoError(lb.Start())

	time.Sleep(smallWaitTime)
	assert.EqualError(lb.Health(), "waiting for initial update")
	lb.Update(controller.IngressUpdate{Entries: []controller.IngressEntry{{
		Host: "james.com",
	}}})
	assert.NoError(lb.Health(), "should be healthy")

	assert.NoError(lb.Stop())
	assert.EqualError(lb.Health(), "nginx is not running")
}

func TestFailsIfNginxDiesEarly(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	lb := newUpdaterWithBinary(tmpDir, "./fake_failing_nginx.sh")

	assert.Error(lb.Start())
	assert.Error(lb.Health())
}

func TestNginxConfig(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	defaultConf := newConf(tmpDir, fakeNginx)

	trustedFrontends := defaultConf
	trustedFrontends.TrustedFrontends = []string{"10.50.185.0/24", "10.82.0.0/16"}

	customLogLevel := defaultConf
	customLogLevel.LogLevel = "info"

	serverNameHashes := defaultConf
	serverNameHashes.ServerNamesHashMaxSize = 128
	serverNameHashes.ServerNamesHashBucketSize = 58

	proxyProtocol := defaultConf
	proxyProtocol.ProxyProtocol = true

	connectTimeout := defaultConf
	connectTimeout.BackendConnectTimeoutSeconds = 3

	enabledAccessLogConf := defaultConf
	enabledAccessLogConf.AccessLog = true
	enabledAccessLogConf.AccessLogDir = "/nginx-access-log"

	logHeadersConf := defaultConf
	logHeadersConf.LogHeaders = []string{"Content-Type", "Authorization"}

	var tests = []struct {
		name             string
		conf             Conf
		expectedSettings []string
	}{
		{
			"can set trusted frontends for real_ip",
			trustedFrontends,
			[]string{
				"set_real_ip_from 10.50.185.0/24;",
				"set_real_ip_from 10.82.0.0/16;",
				"real_ip_recursive on;",
			},
		},
		{
			"can exclude trusted frontends for real_ip",
			defaultConf,
			[]string{
				"!set_real_ip_from",
			},
		},
		{
			"can set log level",
			customLogLevel,
			[]string{
				"error_log stderr info;",
			},
		},
		{
			"default log level is warn",
			defaultConf,
			[]string{
				"error_log stderr warn;",
			},
		},
		{
			"server names hashes are set",
			serverNameHashes,
			[]string{
				"server_names_hash_max_size 128;",
				"server_names_hash_bucket_size 58;",
			},
		},
		{
			"server names hashes not set by default",
			defaultConf,
			[]string{
				"!server_names_hash_max_size",
				"!server_names_hash_bucket_size",
			},
		},
		{
			"PROXY protocol sets real_ip",
			proxyProtocol,
			[]string{
				"real_ip_header proxy_protocol;",
			},
		},
		{
			"PROXY protocol disabled uses X-Forwarded-For header for real_ip",
			defaultConf,
			[]string{
				"real_ip_header X-Forwarded-For;",
			},
		},
		{
			"Proxy connect timeout can be changed",
			connectTimeout,
			[]string{
				"proxy_connect_timeout 3s;",
			},
		},
		{
			"Custom log format is used for access logs",
			defaultConf,
			[]string{
				"log_format upstream_info",
			},
		},
		{
			"Access logs are turned off by default",
			defaultConf,
			[]string{
				"access_log off;",
			},
		},
		{
			"Access logs use custom format when enabled",
			enabledAccessLogConf,
			[]string{
				"access_log /nginx-access-log/access.log upstream_info buffer=32k flush=1m;",
			},
		},
		{
			"Access logs use custom headers when enabled",
			logHeadersConf,
			[]string{
				"\"$request\" $status Content-Type=$http_Content_Type Authorization=$http_Authorization $body_bytes_sent",
			},
		},
	}

	for _, test := range tests {
		fmt.Println(test.name)
		lb := newNginxWithConf(test.conf)

		assert.NoError(lb.Start())
		err := lb.Update(controller.IngressUpdate{})
		assert.NoError(err)

		config, err := ioutil.ReadFile(tmpDir + "/nginx.conf")
		assert.NoError(err)
		configContents := string(config)

		for _, expected := range test.expectedSettings {
			if strings.HasPrefix(expected, "!") {
				assert.NotContains(configContents, expected, "%s\nshould not contain setting", test.name)
			} else {
				assert.Contains(configContents, expected, "%sshould contain setting", test.name)
			}
		}
	}
}

func TestNginxIngressEntries(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	defaultConf := newConf(tmpDir, fakeNginx)
	enableProxyProtocolConf := defaultConf
	enableProxyProtocolConf.ProxyProtocol = true

	var tests = []struct {
		name            string
		config          Conf
		entries         []controller.IngressEntry
		upstreamEntries []string
		serverEntries   []string
	}{
		{
			"Check full ingress entry works",
			defaultConf,
			[]controller.IngressEntry{
				{
					Host:                    "chris.com",
					Namespace:               "core",
					Name:                    "chris-ingress",
					Path:                    "/path",
					ServiceAddress:          "service",
					ServicePort:             8080,
					Allow:                   []string{"10.82.0.0/16"},
					StripPaths:              true,
					BackendKeepAliveSeconds: 1,
				},
				{
					Host:                    "chris.com",
					Namespace:               "core",
					Name:                    "chris-ingress-another",
					Path:                    "/anotherpath",
					ServiceAddress:          "anotherservice",
					ServicePort:             6060,
					Allow:                   []string{"10.86.0.0/16"},
					StripPaths:              false,
					BackendKeepAliveSeconds: 10,
				},
			},
			[]string{
				"    upstream core.anotherservice.6060 {\n" +
					"        server anotherservice:6060;\n" +
					"        keepalive 1024;\n" +
					"    }",
				"    upstream core.service.8080 {\n" +
					"        server service:8080;\n" +
					"        keepalive 1024;\n" +
					"    }",
			},
			[]string{
				"    server {\n" +
					"        listen 9090;\n" +
					"        server_name chris.com;\n" +
					"\n" +
					"        # disable any limits to avoid HTTP 413 for large uploads\n" +
					"        client_max_body_size 0;\n" +
					"\n" +
					"        location /anotherpath/ {\n" +
					"            # Keep original path when proxying.\n" +
					"            proxy_pass http://core.anotherservice.6060;\n" +
					"\n" +
					"            # Set display name for vhost stats.\n" +
					"            vhost_traffic_status_filter_by_set_key /anotherpath/::$proxy_host $server_name;\n" +
					"\n" +
					"            # Close proxy connections after backend keepalive time.\n" +
					"            proxy_read_timeout 10s;\n" +
					"            proxy_send_timeout 10s;\n" +
					"\n" +
					"            # Allow localhost for debugging\n" +
					"            allow 127.0.0.1;\n" +
					"\n" +
					"            # Restrict clients\n" +
					"            allow 10.86.0.0/16;\n" +
					"            \n" +
					"            deny all;\n" +
					"        }\n" +
					"\n" +
					"        location /path/ {\n" +
					"            # Strip location path when proxying.\n" +
					"            # Beware this can cause issues with url encoded characters.\n" +
					"            proxy_pass http://core.service.8080/;\n" +
					"\n" +
					"            # Set display name for vhost stats.\n" +
					"            vhost_traffic_status_filter_by_set_key /path/::$proxy_host $server_name;\n" +
					"\n" +
					"            # Close proxy connections after backend keepalive time.\n" +
					"            proxy_read_timeout 1s;\n" +
					"            proxy_send_timeout 1s;\n" +
					"\n" +
					"            # Allow localhost for debugging\n" +
					"            allow 127.0.0.1;\n" +
					"\n" +
					"            # Restrict clients\n" +
					"            allow 10.82.0.0/16;\n" +
					"            \n" +
					"            deny all;\n" +
					"        }\n" +
					"    }",
			},
		},
		{
			"Check no allows works",
			defaultConf,
			[]controller.IngressEntry{
				{
					Host:           "chris.com",
					Namespace:      "core",
					Name:           "chris-ingress",
					Path:           "/path",
					ServiceAddress: "service",
					ServicePort:    9090,
					Allow:          []string{},
				},
			},
			nil,
			[]string{
				"            # Restrict clients\n" +
					"            \n" +
					"            deny all;\n",
			},
		},
		{
			"Check nil allow works",
			defaultConf,
			[]controller.IngressEntry{
				{
					Host:           "chris.com",
					Namespace:      "core",
					Name:           "chris-ingress",
					Path:           "/path",
					ServiceAddress: "service",
					ServicePort:    9090,
					Allow:          nil,
				},
			},
			nil,
			[]string{
				"            # Restrict clients\n" +
					"            \n" +
					"            deny all;\n",
			},
		},
		{
			"Check servers ordered by hostname",
			defaultConf,
			[]controller.IngressEntry{
				{
					Namespace:      "core",
					Name:           "2-last-ingress",
					Host:           "foo-2.com",
					Path:           "/",
					ServiceAddress: "foo",
					ServicePort:    8080,
					Allow:          []string{"10.82.0.0/16"},
				},
				{
					Namespace:      "core",
					Name:           "0-first-ingress",
					Host:           "foo-0.com",
					Path:           "/",
					ServiceAddress: "foo",
					ServicePort:    8080,
					Allow:          []string{"10.82.0.0/16"},
				},
				{
					Namespace:      "core",
					Name:           "1-next-ingress",
					Host:           "foo-1.com",
					Path:           "/",
					ServiceAddress: "foo",
					ServicePort:    8080,
					Allow:          []string{"10.82.0.0/16"},
				},
			},
			nil,
			[]string{
				"        server_name foo-0.com;",
				"        server_name foo-1.com;",
				"        server_name foo-2.com;",
			},
		},
		{
			"Check path slashes are added correctly",
			defaultConf,
			[]controller.IngressEntry{
				{
					Host:           "chris-0.com",
					Namespace:      "core",
					Name:           "chris-ingress",
					Path:           "",
					ServiceAddress: "service",
					ServicePort:    9090,
				},
				{
					Host:           "chris-1.com",
					Namespace:      "core",
					Name:           "chris-ingress",
					Path:           "/prefix-with-slash/",
					ServiceAddress: "service",
					ServicePort:    9090,
				},
				{
					Host:           "chris-2.com",
					Namespace:      "core",
					Name:           "chris-ingress",
					Path:           "prefix-without-preslash/",
					ServiceAddress: "service",
					ServicePort:    9090,
				},
				{
					Host:           "chris-3.com",
					Namespace:      "core",
					Name:           "chris-ingress",
					Path:           "/prefix-without-postslash",
					ServiceAddress: "service",
					ServicePort:    9090,
				},
				{
					Host:           "chris-4.com",
					Namespace:      "core",
					Name:           "chris-ingress",
					Path:           "prefix-without-anyslash",
					ServiceAddress: "service",
					ServicePort:    9090,
				},
			},
			nil,
			[]string{
				"        location / {\n",
				"        location /prefix-with-slash/ {\n",
				"        location /prefix-without-preslash/ {\n",
				"        location /prefix-without-postslash/ {\n",
				"        location /prefix-without-anyslash/ {\n",
			},
		},
		{
			"Check multiple allows work",
			defaultConf,
			[]controller.IngressEntry{
				{
					Host:           "chris.com",
					Namespace:      "core",
					Name:           "chris-ingress",
					Path:           "",
					ServiceAddress: "service",
					ServicePort:    9090,
					Allow:          []string{"10.82.0.0/16", "10.99.0.0/16"},
				},
			},
			nil,
			[]string{
				"            # Restrict clients\n" +
					"            allow 10.82.0.0/16;\n" +
					"            allow 10.99.0.0/16;\n" +
					"            \n" +
					"            deny all;\n",
			},
		},
		{
			"Duplicate host and paths will only keep the first one",
			defaultConf,
			[]controller.IngressEntry{
				{
					Host:                    "chris.com",
					Namespace:               "core",
					Name:                    "chris-ingress-first",
					Path:                    "/my-path",
					ServiceAddress:          "service1",
					ServicePort:             9090,
					BackendKeepAliveSeconds: 28,
				},
				{
					Host:                    "chris.com",
					Namespace:               "core",
					Name:                    "chris-ingress-second",
					Path:                    "/my-path",
					ServiceAddress:          "service2",
					ServicePort:             9090,
					BackendKeepAliveSeconds: 28,
				},
				{
					Host:                    "chris.com",
					Namespace:               "core",
					Name:                    "chris-ingress-third",
					Path:                    "/my-path",
					ServiceAddress:          "service3",
					ServicePort:             9090,
					BackendKeepAliveSeconds: 28,
				},
				{
					Host:                    "chris-again.com",
					Namespace:               "core",
					Name:                    "chris-ingress-again",
					Path:                    "/my-path",
					ServiceAddress:          "service4",
					ServicePort:             9090,
					BackendKeepAliveSeconds: 28,
				},
			},
			nil,
			[]string{
				"    server {\n" +
					"        listen 9090;\n" +
					"        server_name chris-again.com;\n" +
					"\n" +
					"        # disable any limits to avoid HTTP 413 for large uploads\n" +
					"        client_max_body_size 0;\n" +
					"\n" +
					"        location /my-path/ {\n" +
					"            # Keep original path when proxying.\n" +
					"            proxy_pass http://core.service4.9090;\n" +
					"\n" +
					"            # Set display name for vhost stats.\n" +
					"            vhost_traffic_status_filter_by_set_key /my-path/::$proxy_host $server_name;\n" +
					"\n" +
					"            # Close proxy connections after backend keepalive time.\n" +
					"            proxy_read_timeout 28s;\n" +
					"            proxy_send_timeout 28s;\n" +
					"\n" +
					"            # Allow localhost for debugging\n" +
					"            allow 127.0.0.1;\n" +
					"\n" +
					"            # Restrict clients\n" +
					"            \n" +
					"            deny all;\n" +
					"        }\n" +
					"    }",
				"    server {\n" +
					"        listen 9090;\n" +
					"        server_name chris.com;\n" +
					"\n" +
					"        # disable any limits to avoid HTTP 413 for large uploads\n" +
					"        client_max_body_size 0;\n" +
					"\n" +
					"        location /my-path/ {\n" +
					"            # Keep original path when proxying.\n" +
					"            proxy_pass http://core.service1.9090;\n" +
					"\n" +
					"            # Set display name for vhost stats.\n" +
					"            vhost_traffic_status_filter_by_set_key /my-path/::$proxy_host $server_name;\n" +
					"\n" +
					"            # Close proxy connections after backend keepalive time.\n" +
					"            proxy_read_timeout 28s;\n" +
					"            proxy_send_timeout 28s;\n" +
					"\n" +
					"            # Allow localhost for debugging\n" +
					"            allow 127.0.0.1;\n" +
					"\n" +
					"            # Restrict clients\n" +
					"            \n" +
					"            deny all;\n" +
					"        }\n" +
					"    }",
			},
		},
		{
			"Only a single upstream per ingress",
			defaultConf,
			[]controller.IngressEntry{
				{
					Host:           "chris.com",
					Namespace:      "core",
					Name:           "chris-ingress",
					Path:           "/my-path",
					ServiceAddress: "service",
					ServicePort:    9090,
				},

				{
					Host:           "chris.com",
					Namespace:      "core",
					Name:           "chris-ingress",
					Path:           "/my-path2",
					ServiceAddress: "service",
					ServicePort:    9090,
				},
			},

			[]string{
				"    upstream core.service.9090 {\n" +
					"        server service:9090;\n" +
					"        keepalive 1024;\n" +
					"    }",
			},
			nil,
		},
		{
			"Disabled path stripping should not put a trailing slash on proxy_pass",
			defaultConf,
			[]controller.IngressEntry{
				{
					Host:           "chris.com",
					Namespace:      "core",
					Name:           "chris-ingress",
					Path:           "/path",
					ServiceAddress: "service",
					ServicePort:    9090,
				},
			},
			nil,
			[]string{
				"    proxy_pass http://core.service.9090;\n",
			},
		},
		{
			"PROXY protocol enables proxy_protocol listeners",
			enableProxyProtocolConf,
			[]controller.IngressEntry{
				{
					Host:           "chris.com",
					Namespace:      "core",
					Name:           "chris-ingress",
					Path:           "/path",
					ServiceAddress: "service",
					ServicePort:    9090,
				},
			},
			nil,
			[]string{
				"listen 9090 proxy_protocol;",
			},
		},
		{
			"Locations should be ordered by path",
			defaultConf,
			[]controller.IngressEntry{
				{
					Host:                    "chris.com",
					Namespace:               "core",
					Name:                    "chris-ingress",
					Path:                    "",
					ServiceAddress:          "service",
					ServicePort:             9090,
					BackendKeepAliveSeconds: 28,
				},
				{
					Host:                    "chris.com",
					Namespace:               "core",
					Name:                    "chris-ingress",
					Path:                    "/lala",
					ServiceAddress:          "service",
					ServicePort:             9090,
					BackendKeepAliveSeconds: 28,
				},
				{
					Host:                    "chris.com",
					Namespace:               "core",
					Name:                    "chris-ingress",
					Path:                    "/01234-hi",
					ServiceAddress:          "service",
					ServicePort:             9090,
					BackendKeepAliveSeconds: 28,
				},
			},
			nil,
			[]string{
				"        location / {\n" +
					"            # Keep original path when proxying.\n" +
					"            proxy_pass http://core.service.9090;\n" +
					"\n" +
					"            # Set display name for vhost stats.\n" +
					"            vhost_traffic_status_filter_by_set_key /::$proxy_host $server_name;\n" +
					"\n" +
					"            # Close proxy connections after backend keepalive time.\n" +
					"            proxy_read_timeout 28s;\n" +
					"            proxy_send_timeout 28s;\n" +
					"\n" +
					"            # Allow localhost for debugging\n" +
					"            allow 127.0.0.1;\n" +
					"\n" +
					"            # Restrict clients\n" +
					"            \n" +
					"            deny all;\n" +
					"        }\n" +
					"\n" +
					"        location /01234-hi/ {\n" +
					"            # Keep original path when proxying.\n" +
					"            proxy_pass http://core.service.9090;\n" +
					"\n" +
					"            # Set display name for vhost stats.\n" +
					"            vhost_traffic_status_filter_by_set_key /01234-hi/::$proxy_host $server_name;\n" +
					"\n" +
					"            # Close proxy connections after backend keepalive time.\n" +
					"            proxy_read_timeout 28s;\n" +
					"            proxy_send_timeout 28s;\n" +
					"\n" +
					"            # Allow localhost for debugging\n" +
					"            allow 127.0.0.1;\n" +
					"\n" +
					"            # Restrict clients\n" +
					"            \n" +
					"            deny all;\n" +
					"        }\n" +
					"\n" +
					"        location /lala/ {\n" +
					"            # Keep original path when proxying.\n" +
					"            proxy_pass http://core.service.9090;\n" +
					"\n" +
					"            # Set display name for vhost stats.\n" +
					"            vhost_traffic_status_filter_by_set_key /lala/::$proxy_host $server_name;\n" +
					"\n" +
					"            # Close proxy connections after backend keepalive time.\n" +
					"            proxy_read_timeout 28s;\n" +
					"            proxy_send_timeout 28s;\n" +
					"\n" +
					"            # Allow localhost for debugging\n" +
					"            allow 127.0.0.1;\n" +
					"\n" +
					"            # Restrict clients\n" +
					"            \n" +
					"            deny all;\n" +
					"        }\n",
			},
		},
	}

	for _, test := range tests {
		fmt.Printf("\n=== test: %s\n", test.name)

		lb := newNginxWithConf(test.config)

		assert.NoError(lb.Start())
		entries := test.entries
		err := lb.Update(controller.IngressUpdate{Entries: entries})
		assert.NoError(err)

		config, err := ioutil.ReadFile(tmpDir + "/nginx.conf")
		assert.NoError(err)
		configContents := string(config)

		if test.upstreamEntries != nil {
			assertConfigEntries(t, test.name, "upstream", `(?sU)(    upstream.+\n    })`, test.upstreamEntries, configContents)
		}
		if test.serverEntries != nil {
			assertConfigEntries(t, test.name, "server", `(?sU)(# ingress:.+server.+\n    })`, test.serverEntries, configContents)
		}
		assert.Nil(lb.Stop())

		if t.Failed() {
			t.FailNow()
		}
	}
}

func assertConfigEntries(t *testing.T, testName, entryName, entryRegex string, expectedEntries []string, configContents string) {
	r := regexp.MustCompile(entryRegex)

	actualEntries := r.FindAllStringSubmatch(configContents, -1)
	if len(expectedEntries) != len(actualEntries) {
		assert.FailNow(t, "wrong number of entries", "%s: Expected %d %s entries but found %d: %v\n", testName,
			len(expectedEntries), entryName, len(actualEntries), actualEntries)
	}

	for i := range expectedEntries {
		expected := expectedEntries[i]
		actualSubgroups := actualEntries[i]
		if len(actualSubgroups) != 2 {
			assert.FailNow(t, "wrong number of subgroups", "%s: Expected exactly one subgroup, but %s had %d", testName,
				entryRegex, len(actualSubgroups))
		}
		actual := actualSubgroups[1]

		diff := calculateDiff(actual, expected)
		assert.Contains(t, actual, expected, "%s: %s entry doesn't match, diff(actual, expected):\n%s", testName,
			entryName, diff)
	}
}

func calculateDiff(actual, expected string) string {
	diff, err := diff([]byte(actual), []byte(expected))
	if err != nil {
		panic(err)
	}
	return string(diff)
}

func TestDoesNotUpdateIfConfigurationHasNotChanged(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)
	lb := newUpdaterWithBinary(tmpDir, "./fake_graceful_nginx.py")

	assert.NoError(lb.Start())

	entries := []controller.IngressEntry{
		{
			Host:           "chris.com",
			Path:           "/path",
			ServiceAddress: "service",
			ServicePort:    9090,
		},
	}

	assert.NoError(lb.Update(controller.IngressUpdate{Entries: entries}))

	config1, err := ioutil.ReadFile(tmpDir + "/nginx.conf")
	assert.NoError(err)

	assert.NoError(lb.Update(controller.IngressUpdate{Entries: entries}))
	config2, err := ioutil.ReadFile(tmpDir + "/nginx.conf")
	assert.NoError(err)

	assert.NoError(lb.Stop())

	assert.Equal(string(config1), string(config2), "configs should be identical")
	time.Sleep(time.Duration(1) * time.Second)
}

func TestRateLimitedForUpdates(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)
	lb := newUpdaterWithBinary(tmpDir, "./fake_graceful_nginx.py")

	assert.NoError(lb.Start())

	entries := []controller.IngressEntry{
		{
			Host:           "chris.com",
			Path:           "/path",
			ServiceAddress: "service",
			ServicePort:    9090,
		},
	}

	updatedEntries := []controller.IngressEntry{
		{
			Host:           "chris.com",
			Path:           "/path",
			ServiceAddress: "something different",
			ServicePort:    9090,
		},
	}

	// initial one should go through synchronously
	assert.NoError(lb.Update(controller.IngressUpdate{Entries: entries}))

	// these two should be merged into one
	assert.NoError(lb.Update(controller.IngressUpdate{Entries: updatedEntries}))
	assert.NoError(lb.Update(controller.IngressUpdate{Entries: updatedEntries}))
	time.Sleep(1 * time.Second)

	assert.NoError(lb.Stop())
}

func TestUpdatesMetricsFromNginxStatusPage(t *testing.T) {
	// given
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)

	ts := stubHealthPort()
	defer ts.Close()

	conf := newConf(tmpDir, fakeNginx)
	conf.HealthPort = getPort(ts)
	lb := newNginxWithConf(conf)

	// when
	assert.NoError(lb.Start())
	time.Sleep(time.Millisecond * 50)

	// then
	assert.Equal(9.0, gaugeValue(connectionGauge))
	assert.Equal(2.0, gaugeValue(readingConnectionsGauge))
	assert.Equal(1.0, gaugeValue(writingConnectionsGauge))
	assert.Equal(8.0, gaugeValue(waitingConnectionsGauge))
	assert.Equal(13287.0, gaugeValue(acceptsGauge))
	assert.Equal(13286.0, gaugeValue(handledGauge))
	assert.Equal(66627.0, gaugeValue(requestsGauge))
}

func stubHealthPort() *httptest.Server {
	statusBody := `Active connections: 9
server accepts handled requests
 13287 13286 66627
Reading: 2 Writing: 1 Waiting: 8
`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/basic_status" {
			fmt.Fprintln(w, statusBody)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func getPort(ts *httptest.Server) int {
	_, port, err := net.SplitHostPort(ts.Listener.Addr().String())
	if err != nil {
		panic(err)
	}
	intPort, err := strconv.Atoi(port)
	if err != nil {
		panic(err)
	}
	return intPort
}

func gaugeValue(g prometheus.Gauge) float64 {
	metricCh := make(chan prometheus.Metric, 1)
	g.Collect(metricCh)
	metric := <-metricCh
	var metricVal dto.Metric
	metric.Write(&metricVal)
	return *metricVal.Gauge.Value
}

func TestFailsToUpdateIfConfigurationIsBroken(t *testing.T) {
	assert := assert.New(t)
	tmpDir := setupWorkDir(t)
	defer os.Remove(tmpDir)
	lb := newUpdaterWithBinary(tmpDir, "./fake_nginx_failing_reload.sh")

	assert.NoError(lb.Start())

	entries := []controller.IngressEntry{
		{
			Host:           "chris.com",
			Path:           "/path",
			ServiceAddress: "service",
			ServicePort:    9090,
		},
	}

	err := lb.Update(controller.IngressUpdate{Entries: entries})
	assert.Contains(err.Error(), "Config check failed")
	assert.Contains(err.Error(), "./fake_nginx_failing_reload.sh -t")
}

func setupWorkDir(t *testing.T) string {
	tmpDir, err := ioutil.TempDir(os.TempDir(), "ingress_lb_test")
	assert.NoError(t, err)
	copyNginxTemplate(t, tmpDir)
	return tmpDir
}

func copyNginxTemplate(t *testing.T, tmpDir string) {
	assert.NoError(t, exec.Command("cp", "nginx.tmpl", tmpDir+"/").Run())
}
