// porterpack is a minimal CLI over the experimental internal/packstore:
// a local-directory, log-structured encrypted object store.
//
// Commands:
//
//	porterpack keygen -out <prefix>
//	porterpack init -store <dir> -recipient <pubfile> [...] [-pack-size N] [-chunk-size N]
//	porterpack put -store <dir> -key <privfile> -recipient <pubfile> [...] -name <artifact> -in <file>
//	porterpack ls -store <dir> -key <privfile>
//	porterpack get -store <dir> -key <privfile> -name <artifact> -out <file>
//	porterpack repo-snapshot -store <dir> -key <privfile> -recipient <pubfile> [...] -name <replica> -repo <path>
//	porterpack repo-restore -store <dir> -key <privfile> -name <replica> -out <path>
//	porterpack mirror -from <dir> (-to <dir> | -to-drive <drive folder path> | -to-s3 <key prefix>)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/CarriedWorldUniverse/porter/internal/packstore/localdir"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "porterpack: "+err.Error())
		os.Exit(1)
	}
}

func usage() error {
	return fmt.Errorf("usage: porterpack <keygen -out <prefix> | " +
		"init -store <dir> -recipient <pubfile>... [-pack-size N] [-chunk-size N] | " +
		"put -store <dir> -key <privfile> -recipient <pubfile>... -name <name> -in <file> | " +
		"ls -store <dir> -key <privfile> | " +
		"get -store <dir> -key <privfile> -name <name> -out <file> | " +
		"repo-snapshot -store <dir> -key <privfile> -recipient <pubfile>... -name <replica> -repo <path> | " +
		"repo-restore -store <dir> -key <privfile> -name <replica> -out <path> | " +
		"mirror -from <dir> (-to <dir> | -to-drive <drive folder path> | -to-s3 <key prefix>)>")
}

// repeatedFlag collects repeated -recipient flags in order.
type repeatedFlag []string

func (r *repeatedFlag) String() string { return fmt.Sprint([]string(*r)) }
func (r *repeatedFlag) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func run(args []string) error {
	if len(args) < 1 {
		return usage()
	}

	switch args[0] {
	case "keygen":
		fs := flag.NewFlagSet("keygen", flag.ExitOnError)
		out := fs.String("out", "", "output prefix; writes <prefix>.key and <prefix>.pub")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *out == "" {
			return usage()
		}
		return cmdKeygen(*out)

	case "init":
		fs := flag.NewFlagSet("init", flag.ExitOnError)
		store := fs.String("store", "", "store directory")
		var recipients repeatedFlag
		fs.Var(&recipients, "recipient", "recipient public key file (repeatable)")
		packSize := fs.Int("pack-size", 2*1024*1024, "pack size in bytes")
		chunkSize := fs.Int("chunk-size", 512*1024, "chunk size in bytes")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *store == "" || len(recipients) == 0 {
			return usage()
		}
		return cmdInit(*store, []string(recipients), *packSize, *chunkSize)

	case "put":
		fs := flag.NewFlagSet("put", flag.ExitOnError)
		store := fs.String("store", "", "store directory")
		keyFile := fs.String("key", "", "recipient private key file")
		var recipients repeatedFlag
		fs.Var(&recipients, "recipient", "recipient public key file (repeatable)")
		name := fs.String("name", "", "artifact name")
		in := fs.String("in", "", "input file")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *store == "" || *keyFile == "" || len(recipients) == 0 || *name == "" || *in == "" {
			return usage()
		}
		return cmdPut(*store, *keyFile, []string(recipients), *name, *in)

	case "ls":
		fs := flag.NewFlagSet("ls", flag.ExitOnError)
		store := fs.String("store", "", "store directory")
		keyFile := fs.String("key", "", "recipient private key file")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *store == "" || *keyFile == "" {
			return usage()
		}
		return cmdLs(*store, *keyFile)

	case "get":
		fs := flag.NewFlagSet("get", flag.ExitOnError)
		store := fs.String("store", "", "store directory")
		keyFile := fs.String("key", "", "recipient private key file")
		name := fs.String("name", "", "artifact name")
		out := fs.String("out", "", "output file")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *store == "" || *keyFile == "" || *name == "" || *out == "" {
			return usage()
		}
		return cmdGet(*store, *keyFile, *name, *out)

	case "repo-snapshot":
		fs := flag.NewFlagSet("repo-snapshot", flag.ExitOnError)
		store := fs.String("store", "", "store directory")
		keyFile := fs.String("key", "", "recipient private key file")
		var recipients repeatedFlag
		fs.Var(&recipients, "recipient", "recipient public key file (repeatable)")
		name := fs.String("name", "", "replica name")
		repo := fs.String("repo", "", "path to the git repository to snapshot")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *store == "" || *keyFile == "" || len(recipients) == 0 || *name == "" || *repo == "" {
			return usage()
		}
		return cmdRepoSnapshot(*store, *keyFile, []string(recipients), *name, *repo)

	case "repo-restore":
		fs := flag.NewFlagSet("repo-restore", flag.ExitOnError)
		store := fs.String("store", "", "store directory")
		keyFile := fs.String("key", "", "recipient private key file")
		name := fs.String("name", "", "replica name")
		out := fs.String("out", "", "output directory (must not exist)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *store == "" || *keyFile == "" || *name == "" || *out == "" {
			return usage()
		}
		return cmdRepoRestore(*store, *keyFile, *name, *out)

	case "mirror":
		fs := flag.NewFlagSet("mirror", flag.ExitOnError)
		from := fs.String("from", "", "source store directory")
		to := fs.String("to", "", "destination store directory (localdir)")
		toDrive := fs.String("to-drive", "", "destination Drive folder path")
		toS3 := fs.String("to-s3", "", "destination S3/R2 key prefix")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		set := 0
		for _, v := range []string{*to, *toDrive, *toS3} {
			if v != "" {
				set++
			}
		}
		if *from == "" || set != 1 {
			return usage()
		}
		switch {
		case *to != "":
			dst, err := localdir.New(*to)
			if err != nil {
				return err
			}
			return cmdMirror(*from, dst, *to)
		case *toDrive != "":
			dst, err := driveBackendFromEnv(context.Background(), *toDrive)
			if err != nil {
				return err
			}
			return cmdMirror(*from, dst, *toDrive)
		default:
			dst, desc, err := s3BackendFromEnv(context.Background(), *toS3)
			if err != nil {
				return err
			}
			return cmdMirror(*from, dst, desc)
		}

	default:
		return usage()
	}
}
