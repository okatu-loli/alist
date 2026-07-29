package cmd

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/alist-org/alist/v3/internal/extension"
	"github.com/spf13/cobra"
)

// pluginCmd is a hidden developer command for producing official plugin
// packages (Step 9.3). It is not part of the operator-facing CLI: hidden so it
// doesn't clutter `alist --help`, and it never touches the server DB.
var pluginCmd = &cobra.Command{
	Use:    "plugin",
	Short:  "Developer tools for building signed plugin packages",
	Hidden: true,
}

var pluginGenKeyCmd = &cobra.Command{
	Use:   "genkey",
	Short: "Generate an ed25519 signing keypair (prints public hex, writes private hex to --out)",
	Run: func(cmd *cobra.Command, args []string) {
		out, _ := cmd.Flags().GetString("out")
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			fmt.Fprintln(os.Stderr, "genkey:", err)
			os.Exit(1)
		}
		if out == "" {
			out = "plugin-signing-key.ed25519"
		}
		if err := os.WriteFile(out, []byte(hex.EncodeToString(priv)+"\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "genkey:", err)
			os.Exit(1)
		}
		fmt.Println("public key (embed in trustroot.go):", hex.EncodeToString(pub))
		fmt.Println("private key written to:", out)
	},
}

var pluginSignCmd = &cobra.Command{
	Use:   "sign --key <priv-hex-file> --dir <package-dir>",
	Short: "Write checksums.txt + signature into a plugin package dir",
	Run: func(cmd *cobra.Command, args []string) {
		keyPath, _ := cmd.Flags().GetString("key")
		dir, _ := cmd.Flags().GetString("dir")
		if keyPath == "" || dir == "" {
			fmt.Fprintln(os.Stderr, "sign: --key and --dir are required")
			os.Exit(1)
		}
		priv, err := loadPrivateKey(keyPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sign:", err)
			os.Exit(1)
		}
		if err := extension.SignDir(dir, priv); err != nil {
			fmt.Fprintln(os.Stderr, "sign:", err)
			os.Exit(1)
		}
		fmt.Println("signed:", dir)
	},
}

var pluginPackCmd = &cobra.Command{
	Use:   "pack --dir <package-dir> --out <package.tar.gz>",
	Short: "Package a (signed) plugin dir into a .tar.gz",
	Run: func(cmd *cobra.Command, args []string) {
		dir, _ := cmd.Flags().GetString("dir")
		out, _ := cmd.Flags().GetString("out")
		if dir == "" || out == "" {
			fmt.Fprintln(os.Stderr, "pack: --dir and --out are required")
			os.Exit(1)
		}
		if err := packDir(dir, out); err != nil {
			fmt.Fprintln(os.Stderr, "pack:", err)
			os.Exit(1)
		}
		fmt.Println("packed:", out)
	},
}

func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("private key is not hex: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key must be %d bytes, got %d", ed25519.PrivateKeySize, len(raw))
	}
	return ed25519.PrivateKey(raw), nil
}

// packDir tars+gzips dir's contents (files at archive root, matching the
// install-time extractor's expectation of plugin.json at the root).
func packDir(dir, out string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
}

func init() {
	RootCmd.AddCommand(pluginCmd)
	pluginCmd.AddCommand(pluginGenKeyCmd)
	pluginCmd.AddCommand(pluginSignCmd)
	pluginCmd.AddCommand(pluginPackCmd)

	pluginGenKeyCmd.Flags().String("out", "", "path to write the private key hex (default plugin-signing-key.ed25519)")
	pluginSignCmd.Flags().String("key", "", "path to the ed25519 private key hex file")
	pluginSignCmd.Flags().String("dir", "", "plugin package directory")
	pluginPackCmd.Flags().String("dir", "", "plugin package directory")
	pluginPackCmd.Flags().String("out", "", "output .tar.gz path")
}
