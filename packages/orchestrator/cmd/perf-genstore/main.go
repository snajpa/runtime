// perf-probe-genstore writes a deterministic migration-input store for the W4
// bench probe: N builds x K kinds with plain (uncompressed) payloads and V4
// headers - the legacy layout `migrate-builds` migrates to V5/framed.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage/header"
)

// storeClass is a preset shape for a W4 probe store.
type storeClass struct {
	builds  int
	sizeMiB int
	kinds   string
}

var storeClasses = map[string]storeClass{
	"tiny":    {builds: 32, sizeMiB: 4, kinds: "rootfs.ext4,memfile"},
	"large":   {builds: 4, sizeMiB: 512, kinds: "rootfs.ext4"},
	"latency": {builds: 64, sizeMiB: 8, kinds: "rootfs.ext4,memfile"},
}

func main() {
	out := flag.String("out", "", "store root (file:// base dir); created if missing")
	builds := flag.Int("builds", 16, "number of builds")
	sizeMiB := flag.Int("size-mib", 4, "payload size per artifact (MiB, block-aligned)")
	kindsFlag := flag.String("kinds", "rootfs.ext4,memfile", "comma-separated kind names")
	fill := flag.Int("fill", 0xAB, "payload fill byte")
	idsFile := flag.String("ids", "", "write the build-id list here (default: <out>/../builds.txt)")
	class := flag.String("class", "", "store preset: tiny | large | latency (explicit flags win)")
	flag.Parse()

	if *class != "" {
		preset, ok := storeClasses[*class]
		if !ok {
			log.Fatalf("-class %q: want tiny | large | latency", *class)
		}

		explicit := map[string]bool{}
		flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

		if !explicit["builds"] {
			*builds = preset.builds
		}

		if !explicit["size-mib"] {
			*sizeMiB = preset.sizeMiB
		}

		if !explicit["kinds"] {
			*kindsFlag = preset.kinds
		}
	}

	if *out == "" || *builds <= 0 || *sizeMiB <= 0 {
		log.Fatal("need -out, -builds>0, -size-mib>0")
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	storageSpec, err := storage.ParseStorageURL("file://" + *out)
	if err != nil {
		log.Fatalf("parse storage url: %v", err)
	}

	provider, err := storage.NewProvider(ctx, storageSpec)
	if err != nil {
		log.Fatalf("provider: %v", err)
	}

	data := bytes.Repeat([]byte{byte(*fill)}, (*sizeMiB)<<20)
	sum := sha256.Sum256(data)

	kinds := strings.Split(*kindsFlag, ",")
	ids := make([]string, 0, *builds)

	for range *builds {
		buildID := uuid.New()
		ids = append(ids, buildID.String())

		for _, kind := range kinds {
			kind = strings.TrimSpace(kind)
			paths := storage.Paths{BuildID: buildID.String()}

			blob, err := provider.OpenBlob(ctx, paths.DataFile(kind, storage.CompressionNone))
			if err != nil {
				log.Fatalf("open blob: %v", err)
			}

			if err := blob.Put(ctx, data); err != nil {
				log.Fatalf("put payload: %v", err)
			}

			metadata := header.NewTemplateMetadata(buildID, 4096, uint64(len(data)))

			spec, err := header.NewHeader(metadata, []header.BuildMap{{
				Offset:             0,
				Length:             uint64(len(data)),
				BuildId:            buildID,
				BuildStorageOffset: 0,
			}})
			if err != nil {
				log.Fatalf("new header: %v", err)
			}

			spec.SetBuild(buildID, header.BuildData{Size: int64(len(data)), Checksum: sum})

			if _, _, _, err := header.StoreHeader(ctx, provider, paths.HeaderFile(kind), spec.CloneForUpload(header.MetadataVersionV4)); err != nil {
				log.Fatalf("store header: %v", err)
			}
		}
	}

	idsPath := *idsFile
	if idsPath == "" {
		idsPath = filepath.Join(filepath.Dir(*out), "builds.txt")
	}

	if err := os.WriteFile(idsPath, []byte(strings.Join(ids, "\n")+"\n"), 0o644); err != nil {
		log.Fatalf("write ids: %v", err)
	}

	fmt.Printf("store: %s builds=%d kinds=%s size=%dMiB total=%dMiB ids=%s\n",
		*out, *builds, *kindsFlag, *sizeMiB, *builds*len(kinds)*(*sizeMiB), idsPath)
}
