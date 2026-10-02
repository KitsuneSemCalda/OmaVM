package qemu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/KitsuneForgering/OmaVM/internal/core"
)

// Host paths, overridable in tests like power.SupplyDir.
var (
	sysRoot           = "/sys"
	devRoot           = "/dev"
	vulkanICDDirs     = []string{"/usr/share/vulkan/icd.d", "/etc/vulkan/icd.d", "/usr/local/share/vulkan/icd.d"}
	libDirs           = []string{"/usr/lib", "/usr/lib64", "/usr/lib/x86_64-linux-gnu", "/usr/local/lib"}
	renderServerPaths = []string{"/usr/lib/virgl_render_server", "/usr/libexec/virgl_render_server", "/usr/lib/x86_64-linux-gnu/virgl_render_server", "/usr/local/libexec/virgl_render_server"}
	// qemuDeviceHelp lists a QEMU device's properties.
	qemuDeviceHelp = func(ctx context.Context, device string) (string, error) {
		out, err := exec.CommandContext(ctx, "qemu-system-x86_64", "-device", device+",help").CombinedOutput()
		return string(out), err
	}
)

// Vulkan in the guest (Venus) is only offered on Mesa's Intel and AMD
// drivers: Venus needs dma-buf sharing from the host driver, and those are
// the drivers it is developed and tested against upstream.
var venusICDPrefixes = map[string][]string{
	"0x8086": {"intel_icd"},
	"0x1002": {"radeon_icd"},
}

var gpuVendorNames = map[string]string{
	"0x8086": "Intel",
	"0x1002": "AMD",
	"0x10de": "NVIDIA",
}

func vendorName(id string) string {
	if name, ok := gpuVendorNames[id]; ok {
		return name
	}
	return "unknown vendor " + id
}

func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func canOpenRW(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

type hostGPU struct {
	addr    string
	vendor  string
	bootVGA bool
	// iommuGroup lists every PCI device sharing this GPU's IOMMU group;
	// empty when the IOMMU is off.
	iommuGroup []string
}

func hostGPUs() []hostGPU {
	devices, err := filepath.Glob(filepath.Join(sysRoot, "bus/pci/devices/*"))
	if err != nil {
		return nil
	}
	var gpus []hostGPU
	for _, dev := range devices {
		if !strings.HasPrefix(readTrimmed(filepath.Join(dev, "class")), "0x03") {
			continue
		}
		gpu := hostGPU{
			addr:    filepath.Base(dev),
			vendor:  readTrimmed(filepath.Join(dev, "vendor")),
			bootVGA: readTrimmed(filepath.Join(dev, "boot_vga")) == "1",
		}
		if members, err := os.ReadDir(filepath.Join(dev, "iommu_group/devices")); err == nil {
			for _, m := range members {
				gpu.iommuGroup = append(gpu.iommuGroup, m.Name())
			}
		}
		gpus = append(gpus, gpu)
	}
	return gpus
}

// renderNode returns the first DRM render node this user can open and its
// GPU vendor.
func renderNode() (node, vendor string) {
	nodes, _ := filepath.Glob(filepath.Join(sysRoot, "class/drm/renderD*"))
	for _, n := range nodes {
		dev := filepath.Join(devRoot, "dri", filepath.Base(n))
		if canOpenRW(dev) {
			return dev, readTrimmed(filepath.Join(n, "device/vendor"))
		}
	}
	return "", ""
}

// vulkanDriverInstalled reports whether a Vulkan ICD from prefixes is
// installed and its library actually exists.
func vulkanDriverInstalled(prefixes []string) bool {
	for _, dir := range vulkanICDDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			matched := false
			for _, p := range prefixes {
				if strings.HasPrefix(e.Name(), p) {
					matched = true
				}
			}
			if !matched {
				continue
			}
			var icd struct {
				ICD struct {
					LibraryPath string `json:"library_path"`
				} `json:"ICD"`
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil || json.Unmarshal(data, &icd) != nil || icd.ICD.LibraryPath == "" {
				continue
			}
			lib := icd.ICD.LibraryPath
			if filepath.IsAbs(lib) {
				if _, err := os.Stat(lib); err == nil {
					return true
				}
				continue
			}
			for _, libDir := range libDirs {
				if _, err := os.Stat(filepath.Join(libDir, lib)); err == nil {
					return true
				}
			}
		}
	}
	return false
}

func renderServerInstalled() bool {
	for _, p := range renderServerPaths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// graphicsSupport reports whether Machines can get OpenGL (virgl) and
// Vulkan (Venus) acceleration on this host, with a user-facing reason
// whenever one of them is unavailable.
type graphicsSupport struct {
	openGL       bool
	openGLDetail string
	vulkan       bool
	vulkanDetail string
}

func detectGraphics(ctx context.Context) graphicsSupport {
	var g graphicsSupport
	node, vendor := renderNode()
	help, err := qemuDeviceHelp(ctx, "virtio-vga-gl")
	switch {
	case err != nil:
		g.openGLDetail = "QEMU's accelerated display device (virtio-vga-gl) is not installed"
	case node == "":
		g.openGLDetail = "no GPU render node (/dev/dri/renderD*) is accessible to this user"
	default:
		g.openGL = true
		g.openGLDetail = vendorName(vendor) + " GPU (" + node + ")"
	}
	if !g.openGL {
		g.vulkanDetail = "requires 3D acceleration first"
		return g
	}
	prefixes, supportedVendor := venusICDPrefixes[vendor]
	switch {
	case !strings.Contains(help, "venus="):
		g.vulkanDetail = "this QEMU version has no Venus support"
	case !supportedVendor:
		g.vulkanDetail = "only Intel and AMD GPUs with Mesa drivers are supported, found " + vendorName(vendor)
	case !vulkanDriverInstalled(prefixes):
		g.vulkanDetail = "the host has no Mesa Vulkan driver for its " + vendorName(vendor) + " GPU (install vulkan-intel or vulkan-radeon)"
	case !renderServerInstalled():
		g.vulkanDetail = "virglrenderer was built without its render server (virgl_render_server)"
	case !canOpenRW(filepath.Join(devRoot, "udmabuf")):
		g.vulkanDetail = "/dev/udmabuf is missing or not writable (load the udmabuf module; access usually needs the kvm group)"
	default:
		g.vulkan = true
		g.vulkanDetail = "Venus on the " + vendorName(vendor) + " GPU; the guest needs Mesa's Venus driver"
	}
	return g
}

func passthroughCapability() core.HostCapability {
	c := core.HostCapability{ID: "gpu-passthrough", Label: "Dedicated GPU for a Machine"}
	gpus := hostGPUs()
	switch {
	case len(gpus) == 0:
		c.Detail = "no GPU found"
		return c
	case len(gpus) == 1:
		c.Detail = "this computer has a single GPU (" + vendorName(gpus[0].vendor) + "), which the host display needs"
		return c
	}
	iommu, _ := os.ReadDir(filepath.Join(sysRoot, "kernel/iommu_groups"))
	if len(iommu) == 0 {
		c.Detail = fmt.Sprintf("%d GPUs found, but the IOMMU is disabled", len(gpus))
		c.Hint = "Enable VT-d/AMD-Vi in the firmware and intel_iommu=on or amd_iommu=on on the kernel command line"
		return c
	}
	for _, gpu := range gpus {
		if gpu.bootVGA {
			continue
		}
		// Isolated: every group member is a function of the GPU's own slot
		// (typically its HDMI audio), so it can be handed over as a unit.
		slot := strings.TrimSuffix(gpu.addr, filepath.Ext(gpu.addr))
		isolated := true
		for _, m := range gpu.iommuGroup {
			if strings.TrimSuffix(m, filepath.Ext(m)) != slot {
				isolated = false
			}
		}
		if isolated {
			c.Available = true
			c.Detail = vendorName(gpu.vendor) + " GPU " + gpu.addr + " is in its own IOMMU group"
			c.Hint = "OmaVM does not set up VFIO: binding the GPU to vfio-pci is a manual, privileged host change"
			return c
		}
	}
	c.Detail = "every secondary GPU shares its IOMMU group with other devices"
	return c
}

// InspectHost implements core.HostInspector. It only reads the host; it
// never loads modules, binds drivers or changes permissions.
func (b *Backend) InspectHost(ctx context.Context) ([]core.HostCapability, error) {
	kvm := core.HostCapability{ID: "kvm", Label: "Hardware virtualization (KVM)", Available: canOpenRW(filepath.Join(devRoot, "kvm"))}
	if kvm.Available {
		kvm.Detail = "/dev/kvm is accessible"
	} else {
		kvm.Detail = "/dev/kvm is missing or not accessible to this user"
		kvm.Hint = "Enable virtualization in the firmware and add your user to the kvm group"
	}
	g := detectGraphics(ctx)
	return []core.HostCapability{
		kvm,
		{ID: "graphics-opengl", Label: "3D acceleration (OpenGL)", Available: g.openGL, Detail: g.openGLDetail},
		{ID: "graphics-vulkan", Label: "Vulkan acceleration", Available: g.vulkan, Detail: g.vulkanDetail},
		sshCapability(),
		passthroughCapability(),
	}, nil
}
