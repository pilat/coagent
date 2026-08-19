package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const checksumFile = "checksums.txt"

var versionPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+([-+][0-9A-Za-z][0-9A-Za-z.-]*)?$`)

type options struct {
	version string
	epoch   time.Time
	outDir  string
	license string
	inputs  []string
}

type artifact struct {
	name string
	path string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(opts.outDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	artifacts := make([]artifact, 0, len(opts.inputs))
	for _, input := range opts.inputs {
		item, err := buildArtifact(opts, input)
		if err != nil {
			return err
		}

		artifacts = append(artifacts, item)
	}

	return writeChecksums(opts.outDir, artifacts)
}

func parseOptions(args []string) (options, error) {
	flags := flag.NewFlagSet("releasebuilder", flag.ContinueOnError)
	var opts options
	var epoch int64

	flags.StringVar(&opts.version, "version", "", "release version")
	flags.Int64Var(&epoch, "epoch", 0, "normalized Unix timestamp")
	flags.StringVar(&opts.outDir, "out", "dist", "output directory")
	flags.StringVar(&opts.license, "license", "LICENSE", "license file")

	if err := flags.Parse(args); err != nil {
		return options{}, fmt.Errorf("parse flags: %w", err)
	}

	if !versionPattern.MatchString(opts.version) || epoch <= 0 || flags.NArg() == 0 {
		return options{}, errors.New("version, positive epoch, and platform=binary inputs are required")
	}

	opts.epoch = time.Unix(epoch, 0).UTC()
	opts.inputs = flags.Args()

	return opts, nil
}

func buildArtifact(opts options, input string) (artifact, error) {
	platform, binary, ok := strings.Cut(input, "=")
	if !ok || !supportedPlatform(platform) || binary == "" {
		return artifact{}, fmt.Errorf("invalid platform=binary input %q", input)
	}

	name := "coagent_" + opts.version + "_" + platform + ".tar.gz"
	path := filepath.Join(opts.outDir, name)

	if err := writeArchive(path, binary, opts.license, opts.epoch); err != nil {
		return artifact{}, fmt.Errorf("archive %s: %w", platform, err)
	}

	return artifact{name: name, path: path}, nil
}

func supportedPlatform(platform string) bool {
	switch platform {
	case "linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64":
		return true
	default:
		return false
	}
}

func writeArchive(path, binaryPath, licensePath string, epoch time.Time) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}

	gz := gzip.NewWriter(file)
	gz.ModTime = epoch
	gz.OS = 255
	tw := tar.NewWriter(gz)

	err = addFile(tw, "coagent", binaryPath, 0o755, epoch)
	if err == nil {
		err = addFile(tw, "LICENSE", licensePath, 0o644, epoch)
	}

	return closeArchive(file, gz, tw, err)
}

func addFile(tw *tar.Writer, name, source string, mode int64, epoch time.Time) error {
	data, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer data.Close()

	info, err := data.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", name, err)
	}

	header := &tar.Header{
		Name: name, Mode: mode, Size: info.Size(), ModTime: epoch,
		Uid: 0, Gid: 0, Uname: "root", Gname: "root", Format: tar.FormatUSTAR,
	}
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write %s header: %w", name, err)
	}

	if _, err := io.Copy(tw, data); err != nil {
		return fmt.Errorf("write %s body: %w", name, err)
	}

	return nil
}

func closeArchive(file *os.File, gz *gzip.Writer, tw *tar.Writer, prior error) error {
	for _, closeFn := range []func() error{tw.Close, gz.Close, file.Sync, file.Close} {
		if err := closeFn(); prior == nil && err != nil {
			prior = err
		}
	}

	return prior
}

func writeChecksums(outDir string, artifacts []artifact) error {
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].name < artifacts[j].name })
	var manifest strings.Builder

	for _, item := range artifacts {
		digest, err := fileDigest(item.path)
		if err != nil {
			return err
		}

		manifest.WriteString(digest + "  " + item.name + "\n")
	}

	if err := os.WriteFile(filepath.Join(outDir, checksumFile), []byte(manifest.String()), 0o644); err != nil {
		return fmt.Errorf("write checksum manifest: %w", err)
	}

	return nil
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open archive for checksum: %w", err)
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash archive: %w", err)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}
