package execpath

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const childEnv = "SWARMIA_EXECPATH_TEST_CHILD"

// TestMain makes the test binary, when run with childEnv set, report how it
// sees itself instead of running tests: its executable path after Fix (unless
// childEnv is "nofix"), what it read from stdin, and whether the platform's
// certificate verifier works.
func TestMain(m *testing.M) {
	mode := os.Getenv(childEnv)
	if mode == "" {
		os.Exit(m.Run())
	}
	if mode != "nofix" {
		Fix()
	}
	exe, _ := os.Executable()
	in, _ := io.ReadAll(os.Stdin)
	fmt.Printf("exe=%s\nstdin=%s\nargs=%s\nverify=%v\n", exe, in, strings.Join(os.Args[1:], "|"), verifySelfSigned())
	os.Exit(0)
}

// verifySelfSigned verifies a fresh self-signed certificate with the system
// roots, so on macOS through Security.framework, without any network. It
// fails either way; only the reason tells whether the verifier ran.
func verifySelfSigned() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "example.test"},
		DNSNames:     []string{"example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	_, err = cert.Verify(x509.VerifyOptions{DNSName: "example.test"})
	return err
}

// layout builds, under a temporary directory:
//
//	real/bin/<name>   the file, written by write
//	real/pkg/
//	links/pkg  -> ../real/pkg
//	links/link -> ../real/bin/<name>
//
// so links/pkg/../bin/<name> leads to real/bin/<name> only when links/pkg is
// followed before "..", as with pnpm's node_modules/@swarmia/cli/bin/../../.
func layout(t *testing.T, name string, write func(path string)) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"real/bin", "real/pkg", "links"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "real", "bin", name))
	for link, target := range map[string]string{"links/pkg": "../real/pkg", "links/link": "../real/bin/" + name} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestRealPath(t *testing.T) {
	root := layout(t, "exe", func(path string) {
		if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	})
	exe := filepath.Join(root, "real", "bin", "exe")

	if err := os.MkdirAll(filepath.Join(root, "other", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other", "bin", "exe"), []byte("a different file"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../real/pkg", filepath.Join(root, "other", "link")); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		path string
		want string // "" when the path needs no fixing
	}{
		{"real/bin/exe", ""},
		{"real/pkg/../bin/exe", ""},
		{"links/link", ""},
		{"links/pkg/../../real/bin/exe", ""},
		{"links/pkg/../bin/exe", exe},
		// As text it names a file, but not this one.
		{"other/link/../bin/exe", exe},
		{"links/pkg/../bin/missing", ""},
	} {
		got, ok := realPath(root + "/" + tc.path)
		if tc.want == "" {
			if ok {
				t.Errorf("realPath(%s) = %q, want no fix", tc.path, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("realPath(%s) = %q, %v; want %q", tc.path, got, ok, tc.want)
		}
	}
}

// TestFix runs a copy of the test binary through a path that needs fixing, as
// pnpm's layout gives one, and checks it ends up running from its real path
// with its arguments, stdin and a working certificate verifier.
func TestFix(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := layout(t, "child", func(path string) {
		data, err := os.ReadFile(self)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o755); err != nil {
			t.Fatal(err)
		}
	})
	real := filepath.Join(root, "real", "bin", "child")
	// Not filepath.Join: it would clean the path.
	bad := "links/pkg/../bin/child"

	run := func(t *testing.T, mode, path string) map[string]string {
		t.Helper()
		cmd := exec.Command(path, "a b", "c")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), childEnv+"="+mode)
		cmd.Stdin = strings.NewReader("from stdin")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", path, err, out)
		}
		got := map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			k, v, _ := strings.Cut(line, "=")
			got[k] = v
		}
		return got
	}

	for _, path := range []string{bad, root + "/" + bad} {
		t.Run(path, func(t *testing.T) {
			got := run(t, "fix", path)
			if got["exe"] != real {
				t.Errorf("exe = %q, want %q", got["exe"], real)
			}
			if got["args"] != "a b|c" {
				t.Errorf("args = %q, want %q", got["args"], "a b|c")
			}
			if got["stdin"] != "from stdin" {
				t.Errorf("stdin = %q, want %q", got["stdin"], "from stdin")
			}
			if strings.Contains(got["verify"], "SecPolicyCreateSSL") {
				t.Errorf("certificate verifier broken: %s", got["verify"])
			}
		})
	}

	if runtime.GOOS == "darwin" {
		// Without Fix, the process keeps the path it was started by, so the
		// cases above did exercise it.
		got := run(t, "nofix", root+"/"+bad)
		if got["exe"] != root+"/"+bad {
			t.Errorf("without Fix, exe = %q, want %q", got["exe"], root+"/"+bad)
		}
		t.Logf("without Fix, verify: %s", got["verify"])
	}
}
