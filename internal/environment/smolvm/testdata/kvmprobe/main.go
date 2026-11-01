// Command kvmprobe reports whether this machine can start a KVM guest of
// its own: it opens /dev/kvm and creates a VM there. The gated nested
// virtualization test builds it, mounts it into a smolvm guest and runs
// it there.
package main

import (
	"fmt"
	"os"
	"syscall"
)

// ioctl requests of the KVM API.
const (
	kvmGetAPIVersion = 0xAE00
	kvmCreateVM      = 0xAE01
)

func main() {
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open /dev/kvm:", err)
		os.Exit(1)
	}
	defer kvm.Close()
	version, _, errno := syscall.Syscall(syscall.SYS_IOCTL, kvm.Fd(), kvmGetAPIVersion, 0)
	if errno != 0 {
		fmt.Fprintln(os.Stderr, "KVM_GET_API_VERSION:", errno)
		os.Exit(1)
	}
	vm, _, errno := syscall.Syscall(syscall.SYS_IOCTL, kvm.Fd(), kvmCreateVM, 0)
	if errno != 0 {
		fmt.Fprintln(os.Stderr, "KVM_CREATE_VM:", errno)
		os.Exit(1)
	}
	_ = syscall.Close(int(vm))
	fmt.Printf("KVM API %d: KVM_CREATE_VM ok\n", version)
}
