// Package crd embeds the CustomResourceDefinitions of this release, as `make manifests` generates them.
// `backflow migrate` reads them to put the version it migrates to into the CRDs that are installed.
package crd

import "embed"

// Bases holds bases/*.yaml.
//
//go:embed bases/*.yaml
var Bases embed.FS
