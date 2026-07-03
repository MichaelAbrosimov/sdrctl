// Package device detects physical SDR dongles.
//
// The primary source is sysfs (/sys/bus/usb/devices): it is instant, needs no
// external tools and exposes serials, which lsusb hides without -v. lsusb is
// used only as human-readable reference output in `sdrctl device`.
package device

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const sysUSBPath = "/sys/bus/usb/devices"

type USBDevice struct {
	SysName   string `json:"sys_name"`
	VendorID  string `json:"vendor_id"`
	ProductID string `json:"product_id"`
	Serial    string `json:"serial,omitempty"`
	Product   string `json:"product,omitempty"`
}

// ScanUSB lists USB devices from sysfs. On hosts without sysfs (macOS dev
// machine) it returns an error; callers treat that as "presence unknown".
func ScanUSB() ([]USBDevice, error) {
	entries, err := os.ReadDir(sysUSBPath)
	if err != nil {
		return nil, fmt.Errorf("usb detection unavailable: %w", err)
	}
	var out []USBDevice
	for _, e := range entries {
		name := e.Name()
		// Skip interface nodes like "1-1:1.0" — only whole devices carry idVendor.
		if strings.Contains(name, ":") {
			continue
		}
		dir := filepath.Join(sysUSBPath, name)
		vendor := readAttr(dir, "idVendor")
		product := readAttr(dir, "idProduct")
		if vendor == "" || product == "" {
			continue
		}
		out = append(out, USBDevice{
			SysName:   name,
			VendorID:  vendor,
			ProductID: product,
			Serial:    readAttr(dir, "serial"),
			Product:   readAttr(dir, "product"),
		})
	}
	return out, nil
}

func readAttr(dir, name string) string {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Match returns devices matching vendor/product ids and, when serial is
// non-empty, the exact serial.
func Match(list []USBDevice, vendorID, productID, serial string) []USBDevice {
	var out []USBDevice
	for _, d := range list {
		if !strings.EqualFold(d.VendorID, vendorID) || !strings.EqualFold(d.ProductID, productID) {
			continue
		}
		if serial != "" && d.Serial != serial {
			continue
		}
		out = append(out, d)
	}
	return out
}

// DuplicateSerials returns serials shared by more than one device with the
// given vendor/product ids. RTL-SDR Blog V4 dongles all ship with serial
// "00000001"; without reflashing via rtl_eeprom a multi-device setup cannot
// tell them apart.
func DuplicateSerials(list []USBDevice, vendorID, productID string) []string {
	count := map[string]int{}
	for _, d := range Match(list, vendorID, productID, "") {
		count[d.Serial]++
	}
	var dups []string
	for serial, n := range count {
		if n > 1 {
			dups = append(dups, serial)
		}
	}
	return dups
}

// Lsusb returns raw lsusb output as human-readable reference.
func Lsusb() (string, error) {
	out, err := exec.Command("lsusb").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("lsusb: %w", err)
	}
	return string(out), nil
}
