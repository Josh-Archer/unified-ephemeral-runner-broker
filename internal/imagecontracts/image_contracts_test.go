package imagecontracts

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(repoRoot(t), path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

func TestProviderImagesAreOwnedByRunnerRepo(t *testing.T) {
	cases := map[string][]string{
		"Dockerfile.lambda": {
			"COPY docker/lambda/handler.py /var/task/handler.py",
			"awslambdaric boto3",
			"nodejs",
			"NODE_MAJOR=24",
			"https://deb.nodesource.com/setup_${NODE_MAJOR}.x",
			"UECB_PROVIDER=lambda",
			"RUNNER_ALLOW_RUNASROOT=1",
			"xz-utils",
			"zstd",
		},
		"Dockerfile.cloud-run": {
			"COPY docker/launcher/entrypoint.sh /bootstrap/entrypoint.sh",
			"nodejs",
			"NODE_MAJOR=24",
			"https://deb.nodesource.com/setup_${NODE_MAJOR}.x",
			"UECB_PROVIDER=cloud-run",
			"RUNNER_ALLOW_RUNASROOT=1",
			"xz-utils",
			"zstd",
		},
		"Dockerfile.azure-functions": {
			"COPY docker/azure-functions/function_app.py /home/site/wwwroot/function_app.py",
			"COPY docker/launcher/entrypoint.sh /opt/uecb/runner-entrypoint.sh",
			"nodejs",
			"NODE_MAJOR=24",
			"https://deb.nodesource.com/setup_${NODE_MAJOR}.x",
			"xz-utils",
			"zstd",
		},
	}

	for path, required := range cases {
		t.Run(path, func(t *testing.T) {
			content := readFile(t, path)
			for _, needle := range required {
				if !strings.Contains(content, needle) {
					t.Fatalf("%s missing %q", path, needle)
				}
			}
		})
	}
}

func TestLambdaHandlerSetsProviderAndCapturesRunnerLog(t *testing.T) {
	content := readFile(t, "docker/lambda/handler.py")
	for _, needle := range []string{
		`"UECB_PROVIDER": "lambda"`,
		`"RUNNER_ALLOW_RUNASROOT": "1"`,
		`stdout=log_file`,
		`stderr=subprocess.STDOUT`,
		`error={"message": message}`,
	} {
		if !strings.Contains(content, needle) {
			t.Fatalf("lambda handler missing %q", needle)
		}
	}
}

func TestBrokerImageCrossCompilesFromBuildPlatform(t *testing.T) {
	// Multi-arch publish must cross-compile from BUILDPLATFORM. Running the Go
	// toolchain under QEMU for arm64 makes broker publish take hours.
	content := readFile(t, "Dockerfile.broker")
	for _, needle := range []string{
		"FROM --platform=$BUILDPLATFORM golang:",
		"ARG TARGETARCH",
		"GOARCH=${TARGETARCH}",
		"CGO_ENABLED=0",
	} {
		if !strings.Contains(content, needle) {
			t.Fatalf("Dockerfile.broker missing multi-arch contract %q", needle)
		}
	}
}
