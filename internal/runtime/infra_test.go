package runtime

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// tarWith builds an in-memory archive shaped like a CopyFromContainer
// response: one header per entry, regular entries carrying their body.
func tarWith(t *testing.T, entries ...tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hdr := range entries {
		body := []byte(hdr.Name + " body")
		if hdr.Typeflag == tar.TypeReg && hdr.Size == 0 {
			hdr.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(body[:hdr.Size]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestArchivedFile(t *testing.T) {
	t.Parallel()
	t.Run("regular file", func(t *testing.T) {
		t.Parallel()
		got, err := archivedFile(tarWith(t, tar.Header{Name: "hosts", Typeflag: tar.TypeReg}))
		if err != nil || string(got) != "hosts body" {
			t.Fatalf("archivedFile = %q, %v", got, err)
		}
	})
	t.Run("directory refused", func(t *testing.T) {
		t.Parallel()
		_, err := archivedFile(tarWith(t,
			tar.Header{Name: "etc/", Typeflag: tar.TypeDir},
			tar.Header{Name: "etc/hosts", Typeflag: tar.TypeReg}))
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want a refusal, not some child entry's bytes", err)
		}
	})
	t.Run("symlink refused", func(t *testing.T) {
		t.Parallel()
		_, err := archivedFile(tarWith(t, tar.Header{Name: "hosts", Typeflag: tar.TypeSymlink, Linkname: "real"}))
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want a refusal", err)
		}
	})
	t.Run("empty archive", func(t *testing.T) {
		t.Parallel()
		if _, err := archivedFile(tarWith(t)); !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v, want io.EOF", err)
		}
	})
	t.Run("over the cap", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		hdr := tar.Header{Name: "big", Typeflag: tar.TypeReg, Size: maxContainerFileBytes + 1}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := io.CopyN(tw, zeros{}, hdr.Size); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		_, err := archivedFile(&buf)
		if err == nil || !strings.Contains(err.Error(), "cap") {
			t.Fatalf("err = %v, want the size cap, not a truncated read", err)
		}
	})
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// The M9 ingress slice publishes as one binding per port (ADR 0034): each
// map entry must land as an exposed port plus its literal host binding,
// 0.0.0.0 for the slice and loopback for the admin entry.
func TestPortBindingsIngressSlice(t *testing.T) {
	t.Parallel()
	ports := map[string]string{
		"20000/tcp": "0.0.0.0:20000",
		"20031/tcp": "0.0.0.0:20031",
		"9090/tcp":  "127.0.0.1:19001",
	}
	exposed, bindings, err := portBindings(ports)
	if err != nil {
		t.Fatal(err)
	}
	if len(exposed) != 3 || len(bindings) != 3 {
		t.Fatalf("exposed %d, bindings %d, want 3 and 3", len(exposed), len(bindings))
	}
	for portProto, hostAddr := range ports {
		var found bool
		for port, bs := range bindings {
			if port.String() != portProto {
				continue
			}
			found = true
			if len(bs) != 1 {
				t.Fatalf("%s has %d bindings, want 1", portProto, len(bs))
			}
			if got := bs[0].HostIP.String() + ":" + bs[0].HostPort; got != hostAddr {
				t.Fatalf("%s bound to %s, want %s", portProto, got, hostAddr)
			}
		}
		if !found {
			t.Fatalf("%s missing from bindings %v", portProto, bindings)
		}
	}
}

func TestPortBindingsRejectsGarbage(t *testing.T) {
	t.Parallel()
	if _, _, err := portBindings(map[string]string{"nope": "0.0.0.0:1"}); err == nil {
		t.Fatal("bad port/proto must error")
	}
	if _, _, err := portBindings(map[string]string{"80/tcp": "localhost:80"}); err == nil {
		t.Fatal("a hostname bind must error, bindings take literal addresses")
	}
	if exposed, bindings, err := portBindings(nil); err != nil || exposed != nil || bindings != nil {
		t.Fatalf("empty ports = %v, %v, %v, want all nil", exposed, bindings, err)
	}
}
