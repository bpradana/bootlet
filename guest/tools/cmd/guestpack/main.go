// Command guestpack builds the bootlet guest from prebuilt Alpine packages:
// it downloads and verifies the pinned apks, extracts the kernel Image, checks
// that the kernel config has everything the emulator relies on, and packs a
// BusyBox initramfs. It needs no cross compiler and runs on any host.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"bootlet/guest/apk"
	"bootlet/guest/cpio"
)

type manifest struct {
	Mirror string
	Kernel struct {
		Package  string
		Sha256   string
		Image    string
		Required []string
		Modules  []string
	}
	Busybox struct {
		Package string
		Sha256  string
		Binary  string
	}
}

func main() {
	manifestPath := flag.String("manifest", "guest/manifest.toml", "guest manifest")
	rootfsDir := flag.String("rootfs", "guest/rootfs", "rootfs overlay directory")
	cacheDir := flag.String("cache", "guest/.cache", "download cache")
	outDir := flag.String("out", "guest/out", "output directory")
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("guestpack: ")

	var m manifest
	if _, err := toml.DecodeFile(*manifestPath, &m); err != nil {
		log.Fatal(err)
	}
	must(os.MkdirAll(*cacheDir, 0o755))
	must(os.MkdirAll(*outDir, 0o755))

	kpkg := fetch(m.Mirror, m.Kernel.Package, m.Kernel.Sha256, *cacheDir)
	kfiles, err := apk.Extract(bytes.NewReader(kpkg), func(name string) bool {
		return name == m.Kernel.Image || strings.HasPrefix(name, "boot/config-") || strings.HasPrefix(name, "lib/modules/")
	})
	must(err)

	image := kernelImage(kfiles[m.Kernel.Image].Data)
	checkConfig(kfiles, m.Kernel.Required)

	bpkg := fetch(m.Mirror, m.Busybox.Package, m.Busybox.Sha256, *cacheDir)
	bfiles, err := apk.Extract(bytes.NewReader(bpkg), func(name string) bool { return name == m.Busybox.Binary })
	must(err)
	busybox, ok := bfiles[m.Busybox.Binary]
	if !ok {
		log.Fatalf("%s not found in %s", m.Busybox.Binary, m.Busybox.Package)
	}

	initrd := buildInitrd(busybox.Data, *rootfsDir, kfiles, m.Kernel.Modules)

	write(filepath.Join(*outDir, "Image"), image)
	write(filepath.Join(*outDir, "initrd.cpio"), initrd)
	write(filepath.Join(*outDir, "Image.gz"), gz(image))
	write(filepath.Join(*outDir, "initrd.cpio.gz"), gz(initrd))
}

// fetch returns the package bytes, downloading into the cache if needed.
func fetch(mirror, pkg, want, cacheDir string) []byte {
	cached := filepath.Join(cacheDir, path.Base(pkg))
	if data, err := os.ReadFile(cached); err == nil && sha(data) == want {
		return data
	}
	url := mirror + "/riscv64/" + path.Base(pkg)
	if dir := path.Dir(pkg); dir != "." {
		url = mirror + "/" + dir + "/riscv64/" + path.Base(pkg)
	}
	log.Printf("downloading %s", url)
	resp, err := http.Get(url)
	must(err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("%s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	must(err)
	if got := sha(data); got != want {
		log.Fatalf("%s: sha256 mismatch: got %s, manifest pins %s", url, got, want)
	}
	must(os.WriteFile(cached, data, 0o644))
	return data
}

// kernelImage normalizes the packaged kernel to a raw RISC-V Image so the
// guest never has to decompress anything at emulated speed.
func kernelImage(data []byte) []byte {
	if len(data) == 0 {
		log.Fatal("kernel image not found in package")
	}
	if data[0] == 0x1f && data[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		must(err)
		data, err = io.ReadAll(zr)
		must(err)
	}
	if len(data) < 0x40 || string(data[0x38:0x3c]) != "RSC\x05" {
		log.Fatal("kernel is not a raw RISC-V Image (missing RSC\\x05 magic); EFI zboot images are not supported")
	}
	return data
}

func checkConfig(files map[string]apk.File, required []string) {
	var config []byte
	for name, f := range files {
		if strings.HasPrefix(name, "boot/config-") {
			config = f.Data
		}
	}
	if config == nil {
		log.Fatal("kernel config not found in package")
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(config), "\n") {
		if k, ok := strings.CutSuffix(line, "=y"); ok {
			set[strings.TrimPrefix(k, "CONFIG_")] = true
		}
	}
	var missing []string
	for _, opt := range required {
		if !set[opt] {
			missing = append(missing, "CONFIG_"+opt)
		}
	}
	if len(missing) > 0 {
		log.Fatalf("kernel lacks required built-in options: %s", strings.Join(missing, ", "))
	}
}

func buildInitrd(busybox []byte, rootfsDir string, kfiles map[string]apk.File, modules []string) []byte {
	var buf bytes.Buffer
	w := cpio.NewWriter(&buf)
	for _, d := range []string{"bin", "dev", "etc", "proc", "sys", "tmp", "run", "root", "usr", "mnt", "lib", "lib/modules", "data"} {
		perm := uint32(0o755)
		if d == "tmp" {
			perm = 0o1777
		}
		must(w.Dir(d, perm))
	}
	// Merged /usr: everything lives in /bin.
	must(w.Symlink("sbin", "bin"))
	must(w.Symlink("usr/bin", "../bin"))
	must(w.Symlink("usr/sbin", "../bin"))
	must(w.CharDev("dev/console", 0o600, 5, 1))
	must(w.CharDev("dev/null", 0o666, 1, 3))

	must(w.File("bin/busybox", 0o755, busybox))
	for _, a := range applets(busybox) {
		if a != "busybox" {
			must(w.Symlink("bin/"+a, "busybox"))
		}
	}

	if len(modules) > 0 {
		order := resolveModules(kfiles, modules)
		var list []string
		for _, name := range order {
			data := kfiles[name].Data
			if strings.HasSuffix(name, ".gz") {
				zr, err := gzip.NewReader(bytes.NewReader(data))
				must(err)
				data = must2(io.ReadAll(zr))
			}
			data = stripSignature(data)
			base := strings.TrimSuffix(path.Base(name), ".gz")
			must(w.File("lib/modules/"+base, 0o644, data))
			list = append(list, base)
		}
		// The guest loads these in order with insmod (no modprobe/depmod needed).
		must(w.File("lib/modules/load.list", 0o644, []byte(strings.Join(list, "\n")+"\n")))
	}

	// Overlay last so it can add files under the directories created above.
	must(filepath.WalkDir(rootfsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == rootfsDir {
			return err
		}
		rel := filepath.ToSlash(must2(filepath.Rel(rootfsDir, p)))
		info := must2(d.Info())
		switch {
		case d.IsDir():
			if rel == "etc" {
				return nil // already created
			}
			return w.Dir(rel, 0o755)
		case info.Mode()&fs.ModeSymlink != 0:
			return w.Symlink(rel, must2(os.Readlink(p)))
		default:
			perm := uint32(0o644)
			if info.Mode()&0o111 != 0 {
				perm = 0o755
			}
			return w.File(rel, perm, must2(os.ReadFile(p)))
		}
	}))
	must(w.Close())
	return buf.Bytes()
}

// resolveModules returns the requested modules (paths relative to
// lib/modules/<version>/) and their dependencies in load order, as full
// package paths.
func resolveModules(kfiles map[string]apk.File, wanted []string) []string {
	var prefix string
	var dep []byte
	for name, f := range kfiles {
		if strings.HasSuffix(name, "/modules.dep") {
			prefix, dep = strings.TrimSuffix(name, "modules.dep"), f.Data
		}
	}
	if dep == nil {
		log.Fatal("modules.dep not found in kernel package")
	}
	deps := map[string][]string{}
	for _, line := range strings.Split(string(dep), "\n") {
		if mod, rest, ok := strings.Cut(line, ":"); ok {
			deps[mod] = strings.Fields(rest)
		}
	}
	var order []string
	seen := map[string]bool{}
	var visit func(mod string)
	visit = func(mod string) {
		if seen[mod] {
			return
		}
		seen[mod] = true
		ds, ok := deps[mod]
		if !ok {
			log.Fatalf("module %s not found in modules.dep", mod)
		}
		// modules.dep lists dependencies with the deepest last.
		for i := len(ds) - 1; i >= 0; i-- {
			visit(ds[i])
		}
		if _, ok := kfiles[prefix+mod]; !ok {
			log.Fatalf("module file %s missing", prefix+mod)
		}
		order = append(order, prefix+mod)
	}
	for _, mod := range wanted {
		visit(mod)
	}
	return order
}

// stripSignature removes an appended module signature. Verifying it costs
// the guest a SHA-256 over the whole module plus an RSA check at emulated
// speed, and the kernel does not enforce signatures (MODULE_SIG_FORCE=n).
func stripSignature(mod []byte) []byte {
	const magic = "~Module signature appended~\n"
	if !bytes.HasSuffix(mod, []byte(magic)) {
		return mod
	}
	// struct module_signature (12 bytes, sig_len big-endian last) precedes the magic.
	end := len(mod) - len(magic)
	if end < 12 {
		return mod
	}
	sigLen := int(binary.BigEndian.Uint32(mod[end-4:]))
	if n := end - 12 - sigLen; n > 0 {
		return mod[:n]
	}
	return mod
}

// applets reads BusyBox's applet name table, a NUL-separated sorted list that
// starts with "[" and "[[", so links can be created at pack time instead of
// running `busybox --install` at emulated speed on every boot.
func applets(bin []byte) []string {
	start := bytes.Index(bin, []byte("[\x00[[\x00"))
	if start < 0 {
		log.Fatal("busybox applet table not found")
	}
	end := bytes.Index(bin[start:], []byte("\x00\x00"))
	names := strings.Split(string(bin[start:start+end]), "\x00")
	if len(names) < 50 {
		log.Fatalf("busybox applet table looks wrong (%d entries)", len(names))
	}
	return names
}

func gz(data []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, err := zw.Write(data)
	must(err)
	must(zw.Close())
	return buf.Bytes()
}

func write(name string, data []byte) {
	must(os.WriteFile(name, data, 0o644))
	log.Printf("wrote %s (%d KiB)", name, len(data)/1024)
}

func sha(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func must2[T any](v T, err error) T {
	must(err)
	return v
}
