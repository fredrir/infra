package kustomize

import (
	"fmt"
	"sync"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Builds are serialized because kyaml initializes its OpenAPI schema in unsynchronized package state.
var builds sync.Mutex

func Build(directory string) ([]byte, error) {
	return BuildFileSystem(filesys.MakeFsOnDisk(), directory)
}

func BuildFileSystem(fileSystem filesys.FileSystem, directory string) ([]byte, error) {
	builds.Lock()
	defer builds.Unlock()
	options := krusty.MakeDefaultOptions()
	options.Reorder = krusty.ReorderOptionUnspecified
	resources, err := krusty.MakeKustomizer(options).Run(fileSystem, directory)
	if err != nil {
		return nil, fmt.Errorf("kustomize %s: %w", directory, err)
	}
	return resources.AsYaml()
}
