package main

import (
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/systemboot/systemboot/pkg/bootconfig"
	"github.com/systemboot/systemboot/pkg/crypto"
	"github.com/systemboot/systemboot/pkg/storage"
)

// List of directories where to look for grub config files. The root dorectory
// of each mountpoint, these folders inside the mountpoint and all subfolders
// of these folders are searched
var (
	GrubSearchDirectories = []string{
		"boot",
		"EFI",
		"efi",
		"grub",
		"grub2",
	}
)

type grubVersion int

var (
	grubV1 grubVersion = 1
	grubV2 grubVersion = 2
)

func isGrubSearchDir(dirname string) bool {
	for _, dir := range GrubSearchDirectories {
		if dirname == dir {
			return true
		}
	}
	return false
}

// ParseGrubCfg parses the content of a grub.cfg and returns a list of
// BootConfig structures, one for each menuentry, in the same order as they
// appear in grub.cfg. All opened kernel and initrd files are relative to
// basedir.
func ParseGrubCfg(ver grubVersion, devices []storage.BlockDev, grubcfg string, basedir string) []bootconfig.BootConfig {
	// This parser sucks. It's not even a parser, it just looks for lines
	// starting with menuentry, linux or initrd.
	// TODO use a parser, e.g. https://github.com/alecthomas/participle
	if ver != grubV1 && ver != grubV2 {
		log.Printf("Warning: invalid GRUB version: %d", ver)
		return nil
	}
	kernelBasedir := basedir
	bootconfigs := make([]bootconfig.BootConfig, 0)
	inMenuEntry := false
	var cfg *bootconfig.BootConfig
	for _, line := range strings.Split(grubcfg, "\n") {
		// remove all leading spaces as they are not relevant for the config
		// line
		line = strings.TrimLeft(line, " ")
		sline := strings.Fields(line)
		if len(sline) == 0 {
			continue
		}
		if sline[0] == "menuentry" {
			// if a "menuentry", start a new boot config
			if cfg != nil {
				// save the previous boot config, if any
				if cfg.IsValid() {
					// only consider valid boot configs, i.e. the ones that have
					// both kernel and initramfs
					bootconfigs = append(bootconfigs, *cfg)
				}
			}
			inMenuEntry = true
			cfg = new(bootconfig.BootConfig)
			name := strings.Join(sline[1:], " ")
			name = strings.Split(name, "--")[0]
			cfg.Name = name
		} else if inMenuEntry {
			// check if location of kernel is at an other partition
			// see https://www.gnu.org/software/grub/manual/grub/html_node/search.html
			if sline[0] == "search" {
				for _, str1 := range sline {
					if str1 == "--set=root" {
						log.Printf("Kernel seems to be on an other partitioin then the grub.cfg file")
						for _, str2 := range sline {
							if len(str2) == 36 && string(str2[8]) == "-" && string(str2[13]) == "-" && string(str2[18]) == "-" && string(str2[23]) == "-" {
								kernelFsUUID := str2
								log.Printf("fs-uuid: %s", kernelFsUUID)
								partitions, err := storage.PartitionsByFsUUID(devices, kernelFsUUID)
								if err != nil {
									log.Printf("Unexpected error while looking up fs uuid: %v", err) // PartitionsByFsUUID does not return an error for now
								} else if len(partitions) == 0 {
									log.Printf("WARNING: No partition found with filesystem uuid:'%s' to load kernel from!", kernelFsUUID) // TODO throw error ?
									continue
								}
								if len(partitions) > 1 {
									log.Printf("WARNING: more than one partition found with the given file. Using the first one")
								}
								dev := partitions[0]
								kernelBasedir = path.Dir(kernelBasedir)
								kernelBasedir = path.Join(kernelBasedir, dev.Name)
								log.Printf("Kernel is on: %s", dev.Name)
							}
						}
					}
				}
			}
			// otherwise look for kernel and initramfs configuration
			if len(sline) < 2 {
				// surely not a valid linux or initrd directive, skip it
				continue
			}
			if sline[0] == "linux" || sline[0] == "linux16" || sline[0] == "linuxefi" {
				kernel := sline[1]
				cmdline := strings.Join(sline[2:], " ")
				cmdline = unquote(ver, cmdline)
				cfg.Kernel = path.Join(kernelBasedir, kernel)
				cfg.KernelArgs = cmdline
			} else if sline[0] == "initrd" || sline[0] == "initrd16" || sline[0] == "initrdefi" {
				initrd := sline[1]
				cfg.Initramfs = path.Join(kernelBasedir, initrd)
			} else if sline[0] == "multiboot" || sline[0] == "multiboot2" {
				multiboot := sline[1]
				cmdline := strings.Join(sline[2:], " ")
				cmdline = unquote(ver, cmdline)
				cfg.Multiboot = path.Join(kernelBasedir, multiboot)
				cfg.MultibootArgs = cmdline
			} else if sline[0] == "module" || sline[0] == "module2" {
				module := sline[1]
				cmdline := strings.Join(sline[2:], " ")
				cmdline = unquote(ver, cmdline)
				module = path.Join(kernelBasedir, module)
				if cmdline != "" {
					module = module + " " + cmdline
				}
				cfg.Modules = append(cfg.Modules, module)
			}
		}
	}
	// append last kernel config if it wasn't already
	if inMenuEntry && cfg.IsValid() {
		bootconfigs = append(bootconfigs, *cfg)
	}
	return bootconfigs
}

func unquote(ver grubVersion, text string) string {
	if ver == grubV2 {
		// if grub2, unquote the string, as directives could be quoted
		// https://www.gnu.org/software/grub/manual/grub/grub.html#Quoting
		// TODO unquote everything, not just \$
		return strings.Replace(text, `\$`, "$", -1)
	}
	// otherwise return the unmodified string
	return text
}

// ScanGrubConfigs looks for grub2 and grub legacy config files in the known
// locations and returns a list of boot configurations.
func ScanGrubConfigs(devices []storage.BlockDev, basedir string) []bootconfig.BootConfig {
	bootconfigs := make([]bootconfig.BootConfig, 0)
	err := filepath.Walk(basedir, func(currentPath string, info os.FileInfo, err error) error {
		if path.Dir(currentPath) == basedir && info.IsDir() && !isGrubSearchDir(path.Base(currentPath)) {
			log.Printf("Skip %s", currentPath)
			return filepath.SkipDir // skip irrelevant toplevel directories
		}
		if info.IsDir() {
			log.Printf("Check %s", currentPath)
			return nil // continue
		}
		cfgname := info.Name()
		if cfgname == "grub.cfg" || cfgname == "grub2.cfg" {
			var ver grubVersion
			if cfgname == "grub.cfg" {
				ver = grubV1
			} else if cfgname == "grub2.cfg" {
				ver = grubV2
			}
			// try parsing
			log.Printf("Trying to read %s", currentPath)
			grubcfg, errRead := ioutil.ReadFile(currentPath)
			if errRead != nil {
				log.Printf("cannot open %s: %v", currentPath, errRead)
				return nil // continue anyway
			}
			crypto.TryMeasureData(crypto.ConfigDataPCR, grubcfg, currentPath)
			cfgs := ParseGrubCfg(ver, devices, string(grubcfg), basedir) // TODO get root dir for cfgs out of grub.cfg instead of taking the curren basedir
			bootconfigs = append(bootconfigs, cfgs...)
		}
		return nil // continue
	})
	if err != nil {
		log.Printf("filepath.Walk error: %v \n", err)
	}
	return bootconfigs
}
