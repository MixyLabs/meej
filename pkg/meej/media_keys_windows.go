package meej

import (
	"fmt"
	"syscall"
	"unsafe"

	"github.com/go-ole/go-ole"
)

var (
	CLSID_ImmersiveShell       = ole.NewGUID("{C2F03A33-21F5-47FA-B4BB-156362A2F239}")
	IID_IServiceProvider       = ole.NewGUID("{6D5140C1-7436-11CE-8034-00AA006009FA}")
	IID_IAudioFlyoutController = ole.NewGUID("{41F9D2FB-7834-4AB6-8B1B-73E74064B465}")
)

type IServiceProvider struct {
	ole.IUnknown
}

type IServiceProviderVtbl struct {
	ole.IUnknownVtbl
	QueryService uintptr
}

func (v *IServiceProvider) VTable() *IServiceProviderVtbl {
	return (*IServiceProviderVtbl)(unsafe.Pointer(v.RawVTable))
}

func (v *IServiceProvider) QueryService(sid, iid *ole.GUID, out unsafe.Pointer) error {
	hr, _, _ := syscall.SyscallN(
		v.VTable().QueryService,
		uintptr(unsafe.Pointer(v)),
		uintptr(unsafe.Pointer(sid)),
		uintptr(unsafe.Pointer(iid)),
		uintptr(out),
	)
	if hr != 0 {
		return ole.NewError(hr)
	}
	return nil
}

type IAudioFlyoutController struct {
	ole.IUnknown
}

type IAudioFlyoutControllerVtbl struct {
	ole.IUnknownVtbl
	ShowFlyout uintptr
}

func (v *IAudioFlyoutController) VTable() *IAudioFlyoutControllerVtbl {
	return (*IAudioFlyoutControllerVtbl)(unsafe.Pointer(v.RawVTable))
}

func (v *IAudioFlyoutController) ShowFlyout(mode, param uint64) error {
	hr, _, _ := syscall.SyscallN(
		v.VTable().ShowFlyout,
		uintptr(unsafe.Pointer(v)),
		uintptr(mode),
		uintptr(param),
	)
	if hr != 0 {
		return ole.NewError(hr)
	}
	return nil
}

func ShowAudioFlyout() error {
	var unk *ole.IUnknown
	unk, err := ole.CreateInstance(
		CLSID_ImmersiveShell,
		IID_IServiceProvider,
	)
	if err != nil {
		return fmt.Errorf("ShowAudioFlyout: %w", err)
	}
	shell := (*IServiceProvider)(unsafe.Pointer(unk))
	defer shell.Release()

	var audio *IAudioFlyoutController
	if err := shell.QueryService(IID_IAudioFlyoutController, IID_IAudioFlyoutController, unsafe.Pointer(&audio)); err != nil {
		return fmt.Errorf("ShowAudioFlyout: %w", err)
	}
	defer audio.Release()

	return audio.ShowFlyout(0, 0)
}
