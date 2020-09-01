package main

import (
	"fmt"
	"github.com/stretchr/testify/require"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveMountDestination(t *testing.T) {
	tmpdir, err := ioutil.TempDir("", "golang.test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpdir)
	err = os.MkdirAll(filepath.Join(tmpdir, "folder1"), 0750)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(tmpdir, "folder2"), 0750)
	require.NoError(t, err)
	err = os.MkdirAll(filepath.Join(tmpdir, "folder3"), 0750)
	require.NoError(t, err)
	err = os.Symlink("/folder2", filepath.Join(tmpdir, "folder1", "f2"))
	require.NoError(t, err)
	err = os.Symlink("/folder3", filepath.Join(tmpdir, "folder2", "f3"))
	require.NoError(t, err)
	err = ioutil.WriteFile(filepath.Join(tmpdir, "folder3", "test.txt"), []byte("hello"), 0640)
	require.NoError(t, err)

	p, err := resolveMountDestination(tmpdir, "/folder1/f2/f3/test.txt")
	require.Equal(t, filepath.Join(tmpdir, "/folder3/test.txt"), p)
	require.NoError(t, err)

	p, err = resolveMountDestination(tmpdir, "/folder1/f2/xxxxx/fooo")
	require.Equal(t, filepath.Join(tmpdir, "/folder2/xxxxx/fooo"), p)
	require.Error(t, err, os.ErrExist)

	p, err = resolveMountDestination(tmpdir, "/folder1/f2/f3/hello.txt")
	require.Equal(t, filepath.Join(tmpdir, "/folder3/hello.txt"), p)
	require.Error(t, err, os.ErrExist)
}

func TestEmitCommandFile(t *testing.T) {
	t.Skip("must be fixed")
	cmd := exec.Command("/bin/sh", "-c", "echo foo\n echo bar\n")
	cmd.Stdout = os.Stdout
	err := cmd.Run()
	if err != nil {
		panic(err)
	}
	specFile := "/tmp/crio-lxc.log.582641.config.json"

	spec, err := readBundleSpec(specFile)
	if err != nil {
		panic(err)
	}

	buf := strings.Builder{}
	buf.WriteString("exec")
	for _, arg := range spec.Process.Args {
		buf.WriteRune(' ')
		buf.WriteRune('"')
		//fmt.Fprintf(&buf, "%s", arg)
		buf.WriteString(arg)
		buf.WriteRune('"')
	}
	fmt.Println(buf.String())
}

func TestIsStaticBinary(t *testing.T) {
	require.NoError(t, isStaticBinary("/bin/zcat"))
	require.Error(t, isStaticBinary("/usr/bin/systemd"))
}

func TestCapabilities(t *testing.T) {
	require.NoError(t, runtimeHasCapabilitySupport("/usr/local/bin/startcontainer"))
	require.Error(t, runtimeHasCapabilitySupport("/bin/zcat"))
}

func TestKernelRelease(t *testing.T) {
	release := "5.8.0-trunk-amd64"
	r, err := ParseUtsnameRelease(release)
	require.NoError(t, err)
	require.Equal(t, "trunk-amd64", r.Suffix)
	require.True(t, r.GreaterEqual(5, 8, 0))
	require.True(t, r.GreaterEqual(4, 9, 0))
	require.False(t, r.GreaterEqual(5, 8, 1))

	release = "5.9.3"
	r, err = ParseUtsnameRelease(release)
	require.NoError(t, err)
	require.Empty(t, r.Suffix)
}
