package client

import (
	"path/filepath"
	"slices"

	rocks "github.com/tarantool/go-luarocks"
)

// rocksTrees returns the rock trees an operation sees, upstream's
// cfg.rocks_trees: cfg.Tree, then cfg.Tarantool.Prefix when it is set. That
// is the list upstream builds for itself when no config file sets one
// (core/cfg.lua: the home tree, <LOCALDIR>/.rocks, then hardcoded.PREFIX),
// with cfg.Tree in the home tree's place, where tt keeps it. Each entry is
// cleaned and a duplicate dropped.
func rocksTrees(cfg rocks.Config) []string {
	list := []string{cfg.Tree}
	if cfg.Tarantool.Prefix != "" {
		list = append(list, cfg.Tarantool.Prefix)
	}

	out := make([]string, 0, len(list))

	for _, tree := range list {
		tree = filepath.Clean(tree)
		if !slices.Contains(out, tree) {
			out = append(out, tree)
		}
	}

	return out
}
