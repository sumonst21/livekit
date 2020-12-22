// +build mage

package main

import (
	"crypto/sha1"
	"fmt"
	"go/build"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/magefile/mage/mg"
	log "github.com/pion/ion-log"
)

const (
	protoChecksumFile = ".checksumproto"
	goChecksumFile    = ".checksumgo"
)

// Default target to run when none is specified
// If not set, running mage will list available targets
var Default = Build
var checksummer = NewChecksummer(".", goChecksumFile, ".go")

func init() {
	checksummer.IgnoredFiles = []string{
		"cmd/server/wire_gen.go",
		"pkg/rtc/mock_test.go",
	}
}

// explicitly reinstall all deps
func Deps() error {
	return installTools(true)
}

// regenerate protobuf
func Proto() error {
	protoChecksummer := NewChecksummer("proto", protoChecksumFile, ".proto")
	if !protoChecksummer.IsChanged() {
		return nil
	}

	fmt.Println("generating protobuf")
	target := "proto/livekit"
	if err := os.MkdirAll(target, 0755); err != nil {
		return err
	}

	protoc, err := getToolPath("protoc")
	if err != nil {
		return err
	}
	protoc_go_path, err := getToolPath("protoc-gen-go")
	if err != nil {
		return err
	}
	twirp_path, err := getToolPath("protoc-gen-twirp")

	// generate model and room
	cmd := exec.Command(protoc,
		"--go_out", target,
		"--twirp_out", target,
		"--go_opt=paths=source_relative",
		"--twirp_opt=paths=source_relative",
		"--plugin=go="+protoc_go_path,
		"--plugin=twirp="+twirp_path,
		"-I=proto",
		"proto/room.proto",
		"proto/model.proto",
	)
	if err := cmd.Run(); err != nil {
		return err
	}

	// generate rtc
	cmd = exec.Command(protoc,
		"--go_out", target,
		"--go_opt=paths=source_relative",
		"--plugin=go="+protoc_go_path,
		"-I=proto",
		"proto/rtc.proto",
	)
	if err := cmd.Run(); err != nil {
		return err
	}

	protoChecksummer.WriteChecksum()
	return nil
}

// builds LiveKit server and cli
func Build() error {
	mg.Deps(Proto, generate)
	if !checksummer.IsChanged() {
		fmt.Println("up to date")
		return nil
	}

	fmt.Println("building...")
	if err := os.MkdirAll("bin", 0755); err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-i", "-o", "../../bin/livekit-server")
	cmd.Dir = "cmd/server"
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}

	cmd = exec.Command("go", "build", "-i", "-o", "../../bin/livekit-cli")
	cmd.Dir = "cmd/cli"
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}

	checksummer.WriteChecksum()
	return nil
}

// cleans up builds
func Clean() {
	fmt.Println("cleaning...")
	os.RemoveAll("bin")
	os.Remove(protoChecksumFile)
	os.Remove(goChecksumFile)
}

// code generation
func generate() error {
	mg.Deps(installDeps)
	if !checksummer.IsChanged() {
		return nil
	}

	fmt.Println("wiring...")
	wire, err := getToolPath("wire")
	if err != nil {
		return err
	}
	cmd := exec.Command(wire)
	cmd.Dir = "cmd/server"
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}

	//cmd = exec.Command(wire)
	//cmd.Dir = "cmd/cli"
	//connectStd(cmd)
	//if err := cmd.Run(); err != nil {
	//	return err
	//}

	fmt.Println("updating mocks...")
	mockgen, err := getToolPath("mockgen")
	if err != nil {
		return err
	}

	cmd = exec.Command(mockgen, "-source", "pkg/rtc/interfaces.go", "-destination", "pkg/rtc/mock_test.go", "-package", "rtc")
	connectStd(cmd)
	return cmd.Run()
}

// implicitly install deps
func installDeps() error {
	return installTools(false)
}

func installTools(force bool) error {
	if _, err := getToolPath("protoc"); err != nil {
		return fmt.Errorf("protoc is required but is not found")
	}

	tools := []string{
		"google.golang.org/protobuf/cmd/protoc-gen-go",
		"github.com/twitchtv/twirp/protoc-gen-twirp",
		"github.com/google/wire/cmd/wire",
		"github.com/golang/mock/mockgen",
	}
	for _, t := range tools {
		if err := installTool(t, force); err != nil {
			return err
		}
	}
	return nil
}

func installTool(url string, force bool) error {
	name := filepath.Base(url)
	if !force {
		_, err := getToolPath(name)
		if err == nil {
			// already installed
			return nil
		}
	}

	fmt.Printf("installing %s\n", name)
	cmd := exec.Command("go", "get", "-u", url)
	connectStd(cmd)
	if err := cmd.Run(); err != nil {
		return err
	}

	// check
	_, err := getToolPath(name)
	return err
}

// helpers

func getToolPath(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	// check under gopath
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		gopath = build.Default.GOPATH
	}
	p := filepath.Join(gopath, "bin", name)
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

func connectStd(cmd *exec.Cmd) {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
}

// A helper checksum library that generates a fast, non-portable checksum over a directory of files
// it's designed as a quick way to bypass
type Checksummer struct {
	dir          string
	file         string
	checksum     string
	allExts      bool
	extMap       map[string]bool
	IgnoredFiles []string
}

func NewChecksummer(dir string, checksumfile string, exts ...string) *Checksummer {
	c := &Checksummer{
		dir:    dir,
		file:   checksumfile,
		extMap: make(map[string]bool),
	}
	if len(exts) == 0 {
		c.allExts = true
	} else {
		for _, ext := range exts {
			c.extMap[ext] = true
		}
	}

	return c
}

func (c *Checksummer) IsChanged() bool {
	// default changed
	if err := c.computeChecksum(); err != nil {
		log.Errorf("could not compute checksum: %v", err)
		return true
	}
	// read
	existing, err := c.ReadChecksum()
	if err != nil {
		// may not be there
		return true
	}

	return existing != c.checksum
}

func (c *Checksummer) ReadChecksum() (string, error) {
	b, err := ioutil.ReadFile(filepath.Join(c.dir, c.file))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (c *Checksummer) WriteChecksum() error {
	if err := c.computeChecksum(); err != nil {
		return err
	}
	return ioutil.WriteFile(filepath.Join(c.dir, c.file), []byte(c.checksum), 0644)
}

func (c *Checksummer) computeChecksum() error {
	if c.checksum != "" {
		return nil
	}

	entries := make([]string, 0)
	ignoredMap := make(map[string]bool)
	for _, f := range c.IgnoredFiles {
		ignoredMap[f] = true
	}
	err := filepath.Walk(c.dir, func(path string, info os.FileInfo, err error) error {
		if path == c.dir {
			return nil
		}
		if strings.HasPrefix(info.Name(), ".") {
			if info.IsDir() {
				return filepath.SkipDir
			} else {
				return nil
			}
		}
		if info.IsDir() {
			entries = append(entries, fmt.Sprintf("%s %s", path, info.ModTime().String()))
		} else if !ignoredMap[path] && (c.allExts || c.extMap[filepath.Ext(info.Name())]) {
			entries = append(entries, fmt.Sprintf("%s %d %d", path, info.Size(), info.ModTime().Unix()))
		}
		return nil
	})
	if err != nil {
		return err
	}

	sort.Strings(entries)

	h := sha1.New()
	for _, e := range entries {
		h.Write([]byte(e))
	}
	c.checksum = fmt.Sprintf("%x", h.Sum(nil))

	return nil
}