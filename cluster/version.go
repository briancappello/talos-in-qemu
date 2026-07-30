// Package cluster brings a booted Talos machine up to a working single-node
// Kubernetes cluster. It is deliberately BOOTSTRAP ONLY: no upgrade, no
// scaling, no steady-state reconciliation. Steady state belongs to
// provider-talos, per crd/talosmachine.yaml — this package exists only because
// provider-talos runs INSIDE a cluster and so cannot create your first one.
// Anything that reconciles a running cluster does not belong here.
package cluster

import (
	"fmt"

	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/gendata"
)

// GeneratorVersion is the Talos version this binary's machinery can generate
// configs for. It is a property of the BUILD, not a constant: gendata.VersionTag
// is a go:embed of a file inside the machinery module, so it moves with the pin
// in go.mod and cannot be left stale by hand. That is the whole reason for
// linking machinery rather than shelling out to talosctl — an ambient binary can
// change underneath us, a linked module cannot.
func GeneratorVersion() string { return gendata.VersionTag }

// CheckVersion refuses to generate a config for an image NEWER than the
// generator.
//
// Machinery's VersionContract is documented backwards-only: v1.13 machinery can
// target 1.0..1.13, never 1.14. The trap is that exceeding it does NOT error —
// ParseContractFromVersion("v1.99.0") returns {1,99}, every XxxSupported()
// predicate returns true because 99 outranks every comparison, and you get a
// plausible config for a Talos that does not exist. Verified empirically, and
// confirmed against a real gap too: machinery v1.9.5 asked to target v1.13.7
// produced a config with no error and no warning.
//
// checked reports whether the comparison actually HAPPENED, and it is a
// separate return rather than a sentinel error because the three outcomes are
// three different things and only two of them fit in an error:
//
//	(true, nil)   the guard ran and the image is not newer
//	(true, err)   the guard ran and REFUSED — err says what to do
//	(false, nil)  the guard could not run; the version is unknown
//
// The last is the dangerous one. A pre-release volume id such as
// TALOS_V1_14_0_ALPHA reads as "" from InspectImageVersion, which disables the
// guard for exactly the images most likely to break config generation. So the
// caller MUST say so out loud, and a second return value is what forces it to:
// a sentinel error can be swallowed by a plain `if err != nil`, whereas
// ignoring this one costs a visible `_`.
func CheckVersion(imageVersion string) (checked bool, err error) {
	return checkVersion(imageVersion, GeneratorVersion())
}

// checkVersion takes the generator version as a parameter so the branch where
// the GENERATOR is unparseable is reachable from a test. It cannot happen for a
// build that compiles today, but an untestable branch is a branch nothing
// defends.
func checkVersion(imageVersion, generatorVersion string) (bool, error) {
	// An unparseable version is UNKNOWN, not a refusal. InspectImageVersion
	// upholds a strict never-error contract — unknown disables the guard rather
	// than blocking an image we merely failed to classify — and hard-erroring
	// here would leak that contract downstream through the guard that consumes
	// it. Empty needs no case of its own: machinery's version regexp rejects it
	// like any other non-version, and TestCheckVersion pins that so a future
	// pin that started accepting "" fails loudly instead of reporting "ok" for
	// an image nobody identified.
	img, err := config.ParseContractFromVersion(imageVersion)
	if err != nil {
		return false, nil
	}
	// Same rule for the other operand, and it must be PARSED rather than using
	// config.TalosVersionCurrent: that is a typed nil, and Greater treats nil as
	// newer than everything, so img.Greater(TalosVersionCurrent) is always
	// false — the guard would compile, read correctly, and never fire.
	gen, err := config.ParseContractFromVersion(generatorVersion)
	if err != nil {
		return false, nil
	}
	// Greater compares major.minor only, so a newer PATCH is deliberately not
	// newer: Talos does not change the config contract within a minor release.
	if img.Greater(gen) {
		return true, fmt.Errorf(`image is Talos %s but this build generates configs for %s

Talos config generation is BACKWARDS compatible only, and exceeding it does not
fail loudly — it silently produces a config for a version that does not exist,
which the node then rejects or, worse, accepts and misbehaves under.

  use an image of %s or older, or rebuild tinq against machinery %s`,
			imageVersion, generatorVersion, generatorVersion, imageVersion)
	}
	return true, nil
}
