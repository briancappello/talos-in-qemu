package platform

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const isoSector = 2048

// Layout shared by every synthesised image. Sectors 0-15 are the system area,
// the PVD lives at 16, and the rest is ours.
const (
	rootLBA   = 19
	bootLBA   = 21
	kernelLBA = 25
	isoLBAs   = 27
)

// Byte offsets the corruption tests aim at. The "." and ".." records are 34
// bytes each, so BOOT is the third record in the root extent.
const (
	pvdOff      = 16 * isoSector
	pvdRootRec  = pvdOff + 156
	rootDirOff  = rootLBA * isoSector
	rootBootRec = rootDirOff + 68
	kernelOff   = kernelLBA * isoSector
)

func isoDirRecord(name string, extent, size uint32, isDir bool) []byte {
	n := []byte(name)
	rl := 33 + len(n)
	if rl%2 != 0 {
		rl++
	}
	b := make([]byte, rl)
	b[0] = byte(rl)
	binary.LittleEndian.PutUint32(b[2:], extent)
	binary.BigEndian.PutUint32(b[6:], extent)
	binary.LittleEndian.PutUint32(b[10:], size)
	binary.BigEndian.PutUint32(b[14:], size)
	if isDir {
		b[25] = 0x02
	}
	binary.LittleEndian.PutUint16(b[28:], 1)
	binary.BigEndian.PutUint16(b[30:], 1)
	b[32] = byte(len(n))
	copy(b[33:], n)
	return b
}

// peKernel is the 512-byte EFI-stub stand-in: a DOS stub whose e_lfanew points
// at a PE signature and a machine word. That is the only thing InspectImageArch
// looks at.
func peKernel(machine uint16) []byte {
	k := make([]byte, 512)
	copy(k, "MZ")
	binary.LittleEndian.PutUint32(k[0x3c:], 0x40)
	copy(k[0x40:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(k[0x44:], machine)
	return k
}

// isoLayout lets a test place the extents itself. The default puts everything
// after the PVD, like a real image; the truncation tests deliberately put the
// kernel BEFORE the directory that names it, so a short read still leaves a
// classifiable kernel reachable.
type isoLayout struct{ root, boot, kernel, sectors uint32 }

var defaultLayout = isoLayout{rootLBA, bootLBA, kernelLBA, isoLBAs}

// buildISOAt is the smallest ISO9660 that exercises the real code path:
// PVD at sector 16 -> root dir -> BOOT dir -> the kernel.
func buildISOAt(kernel []byte, kernelName string, l isoLayout) []byte {
	img := make([]byte, l.sectors*isoSector)

	pvd := img[pvdOff:]
	pvd[0] = 1
	copy(pvd[1:], "CD001")
	pvd[6] = 1
	copy(pvd[156:], isoDirRecord("\x00", l.root, isoSector, true))

	root := img[l.root*isoSector:]
	o := 0
	for _, r := range [][]byte{
		isoDirRecord("\x00", l.root, isoSector, true),
		isoDirRecord("\x01", l.root, isoSector, true),
		isoDirRecord("BOOT", l.boot, isoSector, true),
	} {
		copy(root[o:], r)
		o += len(r)
	}

	boot := img[l.boot*isoSector:]
	o = 0
	for _, r := range [][]byte{
		isoDirRecord("\x00", l.boot, isoSector, true),
		isoDirRecord("\x01", l.root, isoSector, true),
		isoDirRecord(kernelName, l.kernel, uint32(len(kernel)), false),
	} {
		copy(boot[o:], r)
		o += len(r)
	}
	copy(img[l.kernel*isoSector:], kernel)
	return img
}

func buildISO(kernel []byte, kernelName string) []byte {
	return buildISOAt(kernel, kernelName, defaultLayout)
}

func writeImage(t *testing.T, img []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.iso")
	if err := os.WriteFile(p, img, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func synthISO(t *testing.T, machine uint16, kernelName string) string {
	t.Helper()
	return writeImage(t, buildISO(peKernel(machine), kernelName))
}

func TestInspectImageArch(t *testing.T) {
	if got := InspectImageArch(synthISO(t, 0x8664, "VMLINUZ.;1")); got != "amd64" {
		t.Errorf("x86_64 kernel => %q, want amd64", got)
	}
	if got := InspectImageArch(synthISO(t, 0xaa64, "VMLINUZ.;1")); got != "arm64" {
		t.Errorf("aarch64 kernel => %q, want arm64", got)
	}
}

// Unknown must never be an error: it disables the guard, it does not break the
// run. Every one of these is a valid image we simply cannot classify.
func TestInspectImageArchUnknownIsSilent(t *testing.T) {
	dir := t.TempDir()

	notISO := filepath.Join(dir, "raw.img")
	if err := os.WriteFile(notISO, make([]byte, 4*isoSector), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := InspectImageArch(notISO); got != "" {
		t.Errorf("non-ISO => %q, want empty", got)
	}
	if got := InspectImageArch(filepath.Join(dir, "absent.iso")); got != "" {
		t.Errorf("missing file => %q, want empty", got)
	}
	if got := InspectImageArch(synthISO(t, 0x8664, "OTHER.;1")); got != "" {
		t.Errorf("ISO without a kernel => %q, want empty", got)
	}
	if got := InspectImageArch(synthISO(t, 0x1c0, "VMLINUZ.;1")); got != "" {
		t.Errorf("unrecognised PE machine => %q, want empty", got)
	}
}

// A file long enough to reach sector 16 but carrying no ISO magic must be
// rejected THERE. Everything downstream of the magic is laid out like a real
// ISO, so without the CD001 check this classifies as amd64.
func TestInspectImageArchRequiresISOMagic(t *testing.T) {
	img := buildISO(peKernel(0x8664), "VMLINUZ.;1")
	copy(img[pvdOff+1:], "XXXXX")
	if got := InspectImageArch(writeImage(t, img)); got != "" {
		t.Errorf("image without CD001 => %q, want empty", got)
	}
}

// The kernel header parser: each of these is a well-formed ISO whose kernel is
// only subtly wrong, so a missing check in peMachine either misreads the
// machine word or indexes out of bounds.
func TestInspectImageArchKernelHeader(t *testing.T) {
	noMZ := peKernel(0x8664)
	noMZ[0], noMZ[1] = 0, 0

	noPESig := peKernel(0x8664)
	copy(noPESig[0x40:], "XX\x00\x00")

	farLfanew := peKernel(0x8664)
	binary.LittleEndian.PutUint32(farLfanew[0x3c:], 0x7ffffff0)

	zeroLfanew := peKernel(0x8664)
	binary.LittleEndian.PutUint32(zeroLfanew[0x3c:], 0)

	// 0xffffffff is the value that goes NEGATIVE if e_lfanew is widened to a
	// signed int, which is why peMachine compares it unsigned.
	hugeLfanew := peKernel(0x8664)
	binary.LittleEndian.PutUint32(hugeLfanew[0x3c:], 0xffffffff)

	for _, tc := range []struct {
		name   string
		kernel []byte
	}{
		{"no MZ signature", noMZ},
		{"no PE signature", noPESig},
		{"e_lfanew past the header we read", farLfanew},
		{"e_lfanew zero", zeroLfanew},
		{"e_lfanew with every bit set", hugeLfanew},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := InspectImageArch(writeImage(t, buildISO(tc.kernel, "VMLINUZ.;1"))); got != "" {
				t.Errorf("=> %q, want empty", got)
			}
		})
	}
}

// The kernel sits at the end of the file, so the header read comes back short.
// Both cuts land inside a field peMachine indexes: 32 bytes stops before
// e_lfanew itself, 0x41 stops between e_lfanew and the PE signature it points
// at. Neither may panic.
func TestInspectImageArchShortKernelRead(t *testing.T) {
	for _, n := range []int{0, 1, 2, 32, 0x3c, 0x3e, 0x40, 0x41, 0x44, 0x45} {
		t.Run(fmt.Sprintf("%d bytes", n), func(t *testing.T) {
			img := buildISO(peKernel(0x8664), "VMLINUZ.;1")
			if got := InspectImageArch(writeImage(t, img[:kernelOff+n])); got != "" {
				t.Errorf("kernel truncated to %d bytes => %q, want empty", n, got)
			}
		})
	}
}

// Directory-record arithmetic on hostile input. Every mutation here is a length
// or extent field a real (or malicious) image controls, and the walk indexes
// with all of them.
func TestInspectImageArchMalformedDirectories(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(img []byte)
	}{
		{"root extent past EOF", func(img []byte) {
			binary.LittleEndian.PutUint32(img[pvdRootRec+2:], 0xffff0000)
		}},
		{"root length zero", func(img []byte) {
			binary.LittleEndian.PutUint32(img[pvdRootRec+10:], 0)
		}},
		{"root length absurd", func(img []byte) {
			binary.LittleEndian.PutUint32(img[pvdRootRec+10:], 0xffffffff)
		}},
		{"root length truncates the BOOT record", func(img []byte) {
			binary.LittleEndian.PutUint32(img[pvdRootRec+10:], 40)
		}},
		{"record length below the fixed header", func(img []byte) {
			img[rootBootRec] = 20
		}},
		{"record length overruns the extent", func(img []byte) {
			img[rootBootRec] = 255
			binary.LittleEndian.PutUint32(img[pvdRootRec+10:], 100)
		}},
		{"name length overruns the record", func(img []byte) {
			img[rootBootRec+32] = 250
		}},
		{"name length overruns the extent", func(img []byte) {
			img[rootBootRec+32] = 250
			binary.LittleEndian.PutUint32(img[pvdRootRec+10:], 106)
		}},
		{"kernel extent past EOF", func(img []byte) {
			binary.LittleEndian.PutUint32(img[bootLBA*isoSector+68+2:], 0xffff0000)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := buildISO(peKernel(0x8664), "VMLINUZ.;1")
			tc.corrupt(img)
			if got := InspectImageArch(writeImage(t, img)); got != "" {
				t.Errorf("=> %q, want empty", got)
			}
		})
	}
}

// Blunt no-panic sweep: truncations at every interesting boundary and noise
// sprayed over an otherwise valid image. The only assertion is that nothing
// panics and nothing claims an architecture it cannot prove.
func TestInspectImageArchNeverPanics(t *testing.T) {
	base := buildISO(peKernel(0x8664), "VMLINUZ.;1")

	var imgs [][]byte
	for _, n := range []int{0, 1, 5, 6, 100, pvdOff, pvdOff + 1, pvdOff + 6, pvdOff + 160,
		rootDirOff, rootDirOff + 40, bootLBA * isoSector, kernelOff, len(base)} {
		imgs = append(imgs, base[:n])
	}

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 32; i++ {
		img := append([]byte(nil), base...)
		for j := 0; j < 64; j++ {
			img[rng.Intn(len(img))] = byte(rng.Intn(256))
		}
		imgs = append(imgs, img)
	}
	noise := make([]byte, 40*isoSector)
	rng.Read(noise)
	imgs = append(imgs, noise)

	for i, img := range imgs {
		got := InspectImageArch(writeImage(t, img))
		if got != "" && got != "amd64" && got != "arm64" {
			t.Errorf("image %d => %q, want one of \"\", amd64, arm64", i, got)
		}
	}
}

// A hybrid ISO carries boot code in sector 0. When no kernel is found the
// extent falls back to 0, so an unchecked lookup would classify the image off
// that boot sector instead of admitting it does not know.
func TestInspectImageArchIgnoresSectorZero(t *testing.T) {
	img := buildISO(peKernel(0x8664), "OTHER.;1")
	copy(img, peKernel(0x8664))
	if got := InspectImageArch(writeImage(t, img)); got != "" {
		t.Errorf("ISO with a PE in sector 0 and no kernel => %q, want empty", got)
	}
}

// A truncated download. In both cases the bytes that DID arrive are enough to
// walk to a kernel — the kernel sits at a lower extent than the directory that
// names it — so treating the short read as usable data misreports the arch
// rather than giving up. The untruncated image is checked first, otherwise
// these would pass on a fixture that never classified anything.
func TestInspectImageArchRejectsShortReads(t *testing.T) {
	for _, tc := range []struct {
		name   string
		layout isoLayout
		keep   int
	}{
		{"PVD sector", isoLayout{root: 10, boot: 11, kernel: 12, sectors: 20}, pvdOff + 200},
		{"root directory extent", isoLayout{root: 19, boot: 18, kernel: 17, sectors: 21}, 19*isoSector + 106},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := buildISOAt(peKernel(0x8664), "VMLINUZ.;1", tc.layout)
			if got := InspectImageArch(writeImage(t, img)); got != "amd64" {
				t.Fatalf("intact fixture => %q, want amd64 — the truncation below would prove nothing", got)
			}
			if got := InspectImageArch(writeImage(t, img[:tc.keep])); got != "" {
				t.Errorf("truncated to %d bytes => %q, want empty", tc.keep, got)
			}
		})
	}
}

// The directory length is a 32-bit field the image controls and it becomes an
// allocation. Uncapped, this reserves 4 GiB to read a 55 KiB file.
func TestInspectImageArchCapsDirectoryAllocation(t *testing.T) {
	img := buildISO(peKernel(0x8664), "VMLINUZ.;1")
	binary.LittleEndian.PutUint32(img[pvdRootRec+10:], 0xffffffff)
	p := writeImage(t, img)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := InspectImageArch(p)
	runtime.ReadMemStats(&after)

	if got != "" {
		t.Errorf("=> %q, want empty", got)
	}
	// TotalAlloc is cumulative, so the delta cannot underflow.
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
		t.Errorf("allocated %d bytes for a 4 GiB directory claim, want under 64 MiB", grew)
	}
}

// Runs against the real Talos images when present; skipped otherwise so the
// suite stays runnable on any machine.
func TestInspectImageArchRealISOs(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	// Subtests, not a bare loop: t.Skip terminates the whole test, so skipping
	// inside a map range would let one missing ISO silently drop the other
	// assertion — and map order is random, so which one is nondeterministic.
	for _, tc := range []struct{ name, want string }{
		{"talos-v1.9.5-amd64.iso", "amd64"},
		{"talos-v1.9.5-arm64.iso", "arm64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(home, ".hvf", "images", tc.name)
			if _, err := os.Stat(p); err != nil {
				t.Skipf("%s not present", p)
			}
			if got := InspectImageArch(p); got != tc.want {
				t.Errorf("%s => %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}
