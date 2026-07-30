package cluster

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config"
)

// CheckVersion has THREE outcomes, not two, and the third is the dangerous one:
// a version we could not classify disables the guard, and the caller has to be
// able to say so out loud. (checked=false, err=nil) is that state.
func TestCheckVersion(t *testing.T) {
	for _, tc := range []struct {
		name, image string
		wantChecked bool
		wantErr     bool
	}{
		{"same as generator", GeneratorVersion(), true, false},
		{"older minor", "v1.9.5", true, false},
		{"much older", "v1.0.0", true, false},
		// TALOS_V1_09_5 is a real volume id and InspectImageVersion renders it
		// with the leading zero. Machinery parses it; the guard must too.
		{"older minor with a leading zero", "v1.09.5", true, false},
		// The contract is major.minor only, so a newer PATCH is not newer.
		// Talos does not change config contracts in a patch release.
		{"newer patch, same minor", "v1.13.99", true, false},
		// Major outranks minor: a hand-rolled `img.Major > gen.Major ||
		// img.Minor > gen.Minor` passes every other row here and then refuses
		// this one, which is a genuinely OLDER Talos.
		{"older major with a higher minor", "v0.14.0", true, false},
		{"newer minor", "v1.14.0", true, true},
		{"absurdly newer minor", "v1.99.0", true, true},
		{"newer major", "v2.0.0", true, true},
		// A pre-release volume id reads as "" in practice, but if a version
		// ever arrives by another route it must still be refused, not rounded
		// down to its own minor.
		{"pre-release of a newer minor", "v1.14.0-alpha.0", true, true},

		// Unknown: guard disabled, never blocks.
		{"empty means detection failed", "", false, false},
		{"unparseable is unknown, not a refusal", "not-a-version", false, false},
		{"garbage that starts like a version", "vvvv", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checked, err := CheckVersion(tc.image)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckVersion(%q) err=%v, wantErr=%v", tc.image, err, tc.wantErr)
			}
			if checked != tc.wantChecked {
				t.Errorf("CheckVersion(%q) checked=%v, want %v", tc.image, checked, tc.wantChecked)
			}
			// The fourth combination is not a documented outcome: a guard that
			// did not run has nothing to refuse. Asserted rather than assumed.
			if !checked && err != nil {
				t.Errorf("(checked=false, err=%v) must never occur: unknown is not an error state", err)
			}
		})
	}
}

// An unparseable version must be indistinguishable from an absent one. If it
// were an error instead, InspectImageVersion's never-error contract would leak
// downstream through the very guard that consumes it.
func TestUnknownIsIndistinguishableFromEmpty(t *testing.T) {
	emptyChecked, emptyErr := CheckVersion("")
	junkChecked, junkErr := CheckVersion("not-a-version")
	if emptyChecked != junkChecked || (emptyErr == nil) != (junkErr == nil) {
		t.Errorf("empty=(%v,%v) junk=(%v,%v), want identical", emptyChecked, emptyErr, junkChecked, junkErr)
	}
}

// The generator's own version is the other operand, and it can be as
// unparseable as the image's. It cannot happen for a build that compiles today
// (see TestGeneratorVersionParses), but the comparison must degrade the same
// way rather than pretend it ran.
func TestUnparseableGeneratorDisablesTheGuard(t *testing.T) {
	for _, gen := range []string{"", "garbage", "v1"} {
		// v1.99.0 would be refused against a real generator, so if the guard
		// ran at all this returns an error and fails.
		checked, err := checkVersion("v1.99.0", gen)
		if err != nil {
			t.Errorf("checkVersion(%q, %q) err=%v, want nil", "v1.99.0", gen, err)
		}
		if checked {
			t.Errorf("checkVersion(%q, %q) checked=true, want false", "v1.99.0", gen)
		}
	}
}

// Refusal is a completed check, not a skipped one. (false, err) must never
// happen — the caller reads checked to choose its wording, and "skipped" is the
// wrong word for a run that refused.
func TestRefusalCountsAsChecked(t *testing.T) {
	checked, err := CheckVersion("v1.99.0")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !checked {
		t.Error("checked=false on a refusal; a refusal is a check that ran")
	}
}

// The message must name BOTH versions and say what to do — the failure it
// prevents is a config silently generated for a Talos that does not exist.
//
// The assertions are PHRASES, not bare tokens, because the message formats four
// verbs over only two distinct values: check for presence alone and swapping any
// pair of arguments still passes, shipping a guard that tells the user to do the
// exact opposite of the fix.
func TestCheckVersionMessageIsActionable(t *testing.T) {
	_, err := CheckVersion("v1.99.0")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{
		"image is Talos v1.99.0",
		"generates configs for " + GeneratorVersion(),
		"use an image of " + GeneratorVersion() + " or older",
		"rebuild tinq against machinery v1.99.0",
		"silently",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message must contain %q, got: %s", want, msg)
		}
	}
}

func TestGeneratorVersionIsAVersion(t *testing.T) {
	v := GeneratorVersion()
	if !strings.HasPrefix(v, "v1.") {
		t.Errorf("GeneratorVersion() = %q, want a v1.x version", v)
	}
}

// The guard is a comparison of two contracts, so a generator version machinery
// cannot parse would disable it for EVERY image. gendata.VersionTag is a
// go:embed of a file inside the machinery module, so this holds by construction
// for a given pin — this is the check that notices if a future pin breaks it.
func TestGeneratorVersionParses(t *testing.T) {
	if _, err := config.ParseContractFromVersion(GeneratorVersion()); err != nil {
		t.Fatalf("machinery cannot parse its own version %q: %v", GeneratorVersion(), err)
	}
}

// The whole reason for linking machinery instead of shelling out to talosctl is
// that the generator's version is a property of the build. A literal would
// compile, pass every other test here, and then quietly lie the day the pin
// moves. This is what makes that mutation fail.
func TestGeneratorVersionTracksTheMachineryPin(t *testing.T) {
	gomod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	m := regexp.MustCompile(`github\.com/siderolabs/talos/pkg/machinery (v\S+)`).FindSubmatch(gomod)
	if m == nil {
		t.Fatal("machinery is not required by go.mod")
	}
	if pinned := string(m[1]); pinned != GeneratorVersion() {
		t.Errorf("GeneratorVersion() = %q but go.mod pins machinery %q; the version must come from the linked module, not a literal", GeneratorVersion(), pinned)
	}
}
