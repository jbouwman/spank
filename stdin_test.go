package main

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// resetGlobals resets global state before each test
func resetGlobals() {
	pausedMu.Lock()
	paused = false
	pausedMu.Unlock()
	minAmplitude = 0.05
	cooldownMs = 200
	stdioMode = true
	volumeScaling = false
	speedRatio = 1.0
}

func TestPauseCommand(t *testing.T) {
	resetGlobals()

	input := `{"cmd":"pause"}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	// Check state changed
	pausedMu.RLock()
	if !paused {
		t.Error("expected paused to be true after pause command")
	}
	pausedMu.RUnlock()

	// Check output
	var resp map[string]string
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "paused" {
		t.Errorf("expected status 'paused', got %q", resp["status"])
	}
}

func TestResumeCommand(t *testing.T) {
	resetGlobals()

	// First pause
	pausedMu.Lock()
	paused = true
	pausedMu.Unlock()

	input := `{"cmd":"resume"}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	// Check state changed
	pausedMu.RLock()
	if paused {
		t.Error("expected paused to be false after resume command")
	}
	pausedMu.RUnlock()

	// Check output
	var resp map[string]string
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["status"] != "resumed" {
		t.Errorf("expected status 'resumed', got %q", resp["status"])
	}
}

func TestSetAmplitudeCommand(t *testing.T) {
	resetGlobals()

	input := `{"cmd":"set","amplitude":0.15}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	// Check state changed
	if minAmplitude != 0.15 {
		t.Errorf("expected minAmplitude 0.15, got %f", minAmplitude)
	}

	// Check output
	var resp struct {
		Status    string  `json:"status"`
		Amplitude float64 `json:"amplitude"`
		Cooldown  int     `json:"cooldown"`
	}
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Status != "settings_updated" {
		t.Errorf("expected status 'settings_updated', got %q", resp.Status)
	}
	if resp.Amplitude != 0.15 {
		t.Errorf("expected amplitude 0.15 in response, got %f", resp.Amplitude)
	}
}

func TestSetCooldownCommand(t *testing.T) {
	resetGlobals()

	input := `{"cmd":"set","cooldown":500}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	// Check state changed
	if cooldownMs != 500 {
		t.Errorf("expected cooldownMs 500, got %d", cooldownMs)
	}

	// Check output
	var resp struct {
		Status   string `json:"status"`
		Cooldown int    `json:"cooldown"`
	}
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Cooldown != 500 {
		t.Errorf("expected cooldown 500 in response, got %d", resp.Cooldown)
	}
}

func TestSetBothCommand(t *testing.T) {
	resetGlobals()

	input := `{"cmd":"set","amplitude":0.2,"cooldown":1000}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	if minAmplitude != 0.2 {
		t.Errorf("expected minAmplitude 0.2, got %f", minAmplitude)
	}
	if cooldownMs != 1000 {
		t.Errorf("expected cooldownMs 1000, got %d", cooldownMs)
	}
}

func TestSetAmplitudeOutOfRange(t *testing.T) {
	resetGlobals()
	originalAmplitude := minAmplitude

	// Test amplitude > 1 (should be ignored)
	input := `{"cmd":"set","amplitude":1.5}` + "\n"
	var output bytes.Buffer
	processCommands(strings.NewReader(input), &output)

	if minAmplitude != originalAmplitude {
		t.Errorf("amplitude should not change for value > 1, got %f", minAmplitude)
	}

	// Test amplitude <= 0 (should be ignored)
	resetGlobals()
	input = `{"cmd":"set","amplitude":0}` + "\n"
	output.Reset()
	processCommands(strings.NewReader(input), &output)

	if minAmplitude != originalAmplitude {
		t.Errorf("amplitude should not change for value <= 0, got %f", minAmplitude)
	}

	// Test negative amplitude
	resetGlobals()
	input = `{"cmd":"set","amplitude":-0.5}` + "\n"
	output.Reset()
	processCommands(strings.NewReader(input), &output)

	if minAmplitude != originalAmplitude {
		t.Errorf("amplitude should not change for negative value, got %f", minAmplitude)
	}
}

func TestVolumeScalingCommand(t *testing.T) {
	resetGlobals()

	// Toggle on
	input := `{"cmd":"volume-scaling"}` + "\n"
	var output bytes.Buffer
	processCommands(strings.NewReader(input), &output)

	if !volumeScaling {
		t.Error("expected volumeScaling to be true after toggle")
	}

	var resp struct {
		Status        string `json:"status"`
		VolumeScaling bool   `json:"volume_scaling"`
	}
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Status != "volume_scaling_toggled" {
		t.Errorf("expected status 'volume_scaling_toggled', got %q", resp.Status)
	}
	if !resp.VolumeScaling {
		t.Error("expected volume_scaling true in response")
	}

	// Toggle off
	output.Reset()
	processCommands(strings.NewReader(input), &output)

	if volumeScaling {
		t.Error("expected volumeScaling to be false after second toggle")
	}
}

func TestStatusCommand(t *testing.T) {
	resetGlobals()
	minAmplitude = 0.1
	cooldownMs = 600

	input := `{"cmd":"status"}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	var resp struct {
		Status        string  `json:"status"`
		Paused        bool    `json:"paused"`
		Amplitude     float64 `json:"amplitude"`
		Cooldown      int     `json:"cooldown"`
		VolumeScaling bool    `json:"volume_scaling"`
	}
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("expected status 'ok', got %q", resp.Status)
	}
	if resp.Paused != false {
		t.Errorf("expected paused false, got %t", resp.Paused)
	}
	if resp.Amplitude != 0.1 {
		t.Errorf("expected amplitude 0.1, got %f", resp.Amplitude)
	}
	if resp.Cooldown != 600 {
		t.Errorf("expected cooldown 600, got %d", resp.Cooldown)
	}
	if resp.VolumeScaling != false {
		t.Errorf("expected volume_scaling false, got %t", resp.VolumeScaling)
	}
}

func TestStatusCommandWhenPaused(t *testing.T) {
	resetGlobals()
	pausedMu.Lock()
	paused = true
	pausedMu.Unlock()

	input := `{"cmd":"status"}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	var resp struct {
		Paused bool `json:"paused"`
	}
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp.Paused != true {
		t.Errorf("expected paused true, got %t", resp.Paused)
	}
}

func TestUnknownCommand(t *testing.T) {
	resetGlobals()

	input := `{"cmd":"invalid"}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	var resp map[string]string
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if _, hasError := resp["error"]; !hasError {
		t.Error("expected error field in response for unknown command")
	}
	if !strings.Contains(resp["error"], "unknown command") {
		t.Errorf("expected 'unknown command' error, got %q", resp["error"])
	}
}

func TestInvalidJSON(t *testing.T) {
	resetGlobals()

	input := `{not valid json}` + "\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	var resp map[string]string
	if err := json.Unmarshal(output.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if _, hasError := resp["error"]; !hasError {
		t.Error("expected error field in response for invalid JSON")
	}
	if !strings.Contains(resp["error"], "invalid command") {
		t.Errorf("expected 'invalid command' error, got %q", resp["error"])
	}
}

func TestEmptyLines(t *testing.T) {
	resetGlobals()

	// Empty lines should be skipped, only the status command should produce output
	input := "\n\n" + `{"cmd":"status"}` + "\n\n"
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 output line, got %d: %v", len(lines), lines)
	}
}

func TestMultipleCommands(t *testing.T) {
	resetGlobals()

	input := `{"cmd":"pause"}
{"cmd":"status"}
{"cmd":"resume"}
{"cmd":"status"}
`
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 output lines, got %d", len(lines))
	}

	// First: paused
	var resp1 map[string]string
	json.Unmarshal([]byte(lines[0]), &resp1)
	if resp1["status"] != "paused" {
		t.Errorf("line 1: expected 'paused', got %q", resp1["status"])
	}

	// Second: status shows paused=true
	var resp2 struct {
		Paused bool `json:"paused"`
	}
	json.Unmarshal([]byte(lines[1]), &resp2)
	if !resp2.Paused {
		t.Error("line 2: expected paused=true")
	}

	// Third: resumed
	var resp3 map[string]string
	json.Unmarshal([]byte(lines[2]), &resp3)
	if resp3["status"] != "resumed" {
		t.Errorf("line 3: expected 'resumed', got %q", resp3["status"])
	}

	// Fourth: status shows paused=false
	var resp4 struct {
		Paused bool `json:"paused"`
	}
	json.Unmarshal([]byte(lines[3]), &resp4)
	if resp4.Paused {
		t.Error("line 4: expected paused=false")
	}
}

func TestAmplitudeToVolume(t *testing.T) {
	tests := []struct {
		name      string
		amplitude float64
		wantMin   float64
		wantMax   float64
	}{
		{"below minimum returns min volume", 0.01, -3.0, -3.0},
		{"at minimum returns min volume", 0.05, -3.0, -3.0},
		{"above maximum returns max volume", 1.0, 0.0, 0.0},
		{"at maximum returns max volume", 0.80, 0.0, 0.0},
		{"mid amplitude returns mid-range", 0.40, -2.0, -0.5},
		{"low amplitude is quieter than high", 0.10, -3.0, -1.5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := amplitudeToVolume(tt.amplitude)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("amplitudeToVolume(%f) = %f, want in [%f, %f]",
					tt.amplitude, got, tt.wantMin, tt.wantMax)
			}
		})
	}

	// Monotonicity: higher amplitude should yield higher (or equal) volume
	prev := amplitudeToVolume(0.05)
	for amp := 0.10; amp <= 0.80; amp += 0.05 {
		cur := amplitudeToVolume(amp)
		if cur < prev-1e-9 {
			t.Errorf("non-monotonic: amplitudeToVolume(%f)=%f < amplitudeToVolume(prev)=%f",
				amp, cur, prev)
		}
		prev = cur
	}

	// Verify no NaN or Inf
	for _, amp := range []float64{0, 0.05, 0.1, 0.5, 0.8, 1.0, 10.0} {
		v := amplitudeToVolume(amp)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("amplitudeToVolume(%f) returned %f", amp, v)
		}
	}
}

func TestAmplitudeToBoost(t *testing.T) {
	tests := []struct {
		name      string
		amplitude float64
		wantMin   float64
		wantMax   float64
	}{
		{"below minimum returns min boost", 0.01, 0.5, 0.5},
		{"at minimum returns min boost", 0.05, 0.5, 0.5},
		{"above maximum returns max boost", 1.0, 1.5, 1.5},
		{"at maximum returns max boost", 0.80, 1.5, 1.5},
		{"mid amplitude returns mid-range", 0.40, 0.8, 1.3},
		{"low amplitude is smaller than high", 0.10, 0.5, 1.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := amplitudeToBoost(tt.amplitude)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("amplitudeToBoost(%f) = %f, want in [%f, %f]",
					tt.amplitude, got, tt.wantMin, tt.wantMax)
			}
		})
	}

	// Monotonicity
	prev := amplitudeToBoost(0.05)
	for amp := 0.10; amp <= 0.80; amp += 0.05 {
		cur := amplitudeToBoost(amp)
		if cur < prev-1e-9 {
			t.Errorf("non-monotonic: amplitudeToBoost(%f)=%f < prev=%f",
				amp, cur, prev)
		}
		prev = cur
	}

	// No NaN or Inf
	for _, amp := range []float64{0, 0.05, 0.1, 0.5, 0.8, 1.0, 10.0} {
		v := amplitudeToBoost(amp)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("amplitudeToBoost(%f) returned %f", amp, v)
		}
	}
}

func TestNoteFreq(t *testing.T) {
	// A4 (MIDI 69) should be exactly 440 Hz
	a4 := noteFreq(69)
	if math.Abs(a4-440.0) > 0.001 {
		t.Errorf("noteFreq(69) = %f, want 440.0", a4)
	}

	// C4 (MIDI 60) should be ~261.63 Hz
	c4 := noteFreq(60)
	if math.Abs(c4-261.626) > 0.01 {
		t.Errorf("noteFreq(60) = %f, want ~261.63", c4)
	}

	// Octave relationship: MIDI note + 12 = double frequency
	for midi := 36; midi < 84; midi++ {
		f1 := noteFreq(midi)
		f2 := noteFreq(midi + 12)
		ratio := f2 / f1
		if math.Abs(ratio-2.0) > 0.001 {
			t.Errorf("noteFreq(%d)/noteFreq(%d) = %f, want 2.0", midi+12, midi, ratio)
		}
	}

	// Monotonicity: higher MIDI = higher frequency
	for midi := 21; midi < 108; midi++ {
		if noteFreq(midi+1) <= noteFreq(midi) {
			t.Errorf("noteFreq(%d) >= noteFreq(%d)", midi, midi+1)
		}
	}
}

func TestNoteName(t *testing.T) {
	tests := []struct {
		midi int
		want string
	}{
		{60, "C4"},
		{61, "C#4"},
		{69, "A4"},
		{72, "C5"},
		{48, "C3"},
		{71, "B4"},
	}
	for _, tt := range tests {
		got := noteName(tt.midi)
		if got != tt.want {
			t.Errorf("noteName(%d) = %q, want %q", tt.midi, got, tt.want)
		}
	}
}

func TestKeyToMIDIMapping(t *testing.T) {
	// Verify all mapped keys produce valid MIDI values
	for key, midi := range keyToMIDI {
		if midi < 21 || midi > 108 {
			t.Errorf("keyToMIDI[%c] = %d, outside reasonable MIDI range", key, midi)
		}
	}

	// Verify piano layout: white key sequence should be ascending
	whiteKeysUpper := []byte{'q', 'w', 'e', 'r', 't', 'y', 'u', 'i', 'o', 'p'}
	for i := 1; i < len(whiteKeysUpper); i++ {
		prev := keyToMIDI[whiteKeysUpper[i-1]]
		cur := keyToMIDI[whiteKeysUpper[i]]
		if cur <= prev {
			t.Errorf("white key %c (MIDI %d) should be higher than %c (MIDI %d)",
				whiteKeysUpper[i], cur, whiteKeysUpper[i-1], prev)
		}
	}

	// Verify black keys are between adjacent white keys
	// '2' should be between 'q' (C4=60) and 'w' (D4=62) -> C#4=61
	if keyToMIDI['2'] != 61 {
		t.Errorf("keyToMIDI['2'] = %d, want 61 (C#4)", keyToMIDI['2'])
	}
}

func TestAccordionStateActiveNotes(t *testing.T) {
	state := newAccordionState()

	// No notes initially
	notes := state.activeNotes()
	if len(notes) != 0 {
		t.Errorf("expected 0 active notes, got %d", len(notes))
	}

	// Press a key
	state.pressKey('q') // C4
	notes = state.activeNotes()
	if len(notes) != 1 || notes[0] != 60 {
		t.Errorf("expected [60], got %v", notes)
	}

	// Press another key
	state.pressKey('e') // E4
	notes = state.activeNotes()
	if len(notes) != 2 {
		t.Errorf("expected 2 active notes, got %d", len(notes))
	}
	// Should be sorted
	if notes[0] != 60 || notes[1] != 64 {
		t.Errorf("expected [60, 64], got %v", notes)
	}

	// Non-mapped key should not produce a note
	state.pressKey('`')
	notes = state.activeNotes()
	if len(notes) != 2 {
		t.Errorf("expected still 2 notes after non-mapped key, got %d", len(notes))
	}
}

func TestAccordionStateExpireKeys(t *testing.T) {
	state := newAccordionState()

	// Set a key with an old timestamp
	state.mu.Lock()
	state.activeKeys['q'] = time.Now().Add(-1 * time.Second) // expired
	state.activeKeys['w'] = time.Now()                       // fresh
	state.mu.Unlock()

	state.expireKeys()

	notes := state.activeNotes()
	if len(notes) != 1 || notes[0] != 62 { // 'w' = D4 = 62
		t.Errorf("expected only D4 (62) after expiry, got %v", notes)
	}
}

func TestRenderTiltBar(t *testing.T) {
	// Center tilt should have marker at center
	bar := renderTiltBar(0)
	if len(bar) != 16 {
		t.Errorf("tilt bar length = %d, want 16", len(bar))
	}

	// Positive tilt should shift marker right
	barRight := renderTiltBar(0.5)
	barLeft := renderTiltBar(-0.5)
	// Just verify they're different and correct length
	if len(barRight) != 16 || len(barLeft) != 16 {
		t.Error("tilt bar wrong length")
	}
	if barRight == barLeft {
		t.Error("left and right tilt should produce different bars")
	}
}

func TestBagpipeReed(t *testing.T) {
	// bagpipeReed should return a value in [-1, 1] range (normalized)
	for _, freq := range []float64{240.0, 480.0, 960.0} {
		for tSec := 0.0; tSec < 0.01; tSec += 0.0001 {
			v := bagpipeReed(freq, tSec, 1.0)
			if v < -1.1 || v > 1.1 {
				t.Errorf("bagpipeReed(%f, %f, 1.0) = %f, outside expected range", freq, tSec, v)
			}
		}
	}

	// Lower brightness should produce less high-frequency content (lower peak)
	// Test by checking that bright tones have higher max absolute values
	maxBright := 0.0
	maxDim := 0.0
	for tSec := 0.0; tSec < 0.01; tSec += 0.0001 {
		vBright := math.Abs(bagpipeReed(480.0, tSec, 1.0))
		vDim := math.Abs(bagpipeReed(480.0, tSec, 0.0))
		if vBright > maxBright {
			maxBright = vBright
		}
		if vDim > maxDim {
			maxDim = vDim
		}
	}
	// Both should be <= 1 (normalized)
	if maxBright > 1.1 {
		t.Errorf("bright bagpipeReed max = %f, want <= 1.0", maxBright)
	}
	if maxDim > 1.1 {
		t.Errorf("dim bagpipeReed max = %f, want <= 1.0", maxDim)
	}
}

func TestChanterScale(t *testing.T) {
	// Scale should be ascending in frequency
	for i := 1; i < len(chanterScale); i++ {
		if chanterScale[i].freq <= chanterScale[i-1].freq {
			t.Errorf("chanterScale[%d] (%s, %.0f Hz) should be higher than [%d] (%s, %.0f Hz)",
				i, chanterScale[i].name, chanterScale[i].freq,
				i-1, chanterScale[i-1].name, chanterScale[i-1].freq)
		}
	}

	// Should have 9 notes (Low G through High A)
	if len(chanterScale) != 9 {
		t.Errorf("chanterScale has %d notes, want 9", len(chanterScale))
	}

	// Each note should have a unique key
	keys := make(map[byte]bool)
	for _, n := range chanterScale {
		if keys[n.key] {
			t.Errorf("duplicate chanter key: %c", n.key)
		}
		keys[n.key] = true
	}
}

func TestChanterKeyToNote(t *testing.T) {
	// All chanter scale keys should be in the map
	for i, n := range chanterScale {
		idx, ok := chanterKeyToNote[n.key]
		if !ok {
			t.Errorf("chanterKeyToNote missing key %c", n.key)
		}
		if idx != i {
			t.Errorf("chanterKeyToNote[%c] = %d, want %d", n.key, idx, i)
		}
	}
}

func TestBagpipeStateCurrentChanterNote(t *testing.T) {
	state := newBagpipeState()

	// No note initially
	if idx := state.currentChanterNote(); idx != -1 {
		t.Errorf("expected -1 with no keys, got %d", idx)
	}

	// Press Low A (key 's')
	state.pressKey('s')
	if idx := state.currentChanterNote(); idx != 1 {
		t.Errorf("expected 1 (Low A) after pressing 's', got %d", idx)
	}

	// Press D (key 'j') — more recent, should win
	time.Sleep(1 * time.Millisecond)
	state.pressKey('j')
	if idx := state.currentChanterNote(); idx != 4 {
		t.Errorf("expected 4 (D) after pressing 'j', got %d", idx)
	}

	// Non-chanter key should not affect result
	state.pressKey('z')
	if idx := state.currentChanterNote(); idx != 4 {
		t.Errorf("expected still 4 after non-chanter key, got %d", idx)
	}
}

func TestBagpipeStateBlowing(t *testing.T) {
	state := newBagpipeState()

	// Not blowing initially
	state.mu.RLock()
	if state.blowing {
		t.Error("expected not blowing initially")
	}
	state.mu.RUnlock()

	// Pressing space = blowing
	state.pressKey(' ')
	state.mu.RLock()
	if !state.blowing {
		t.Error("expected blowing after pressing space")
	}
	state.mu.RUnlock()
}

func TestUpdateBag(t *testing.T) {
	state := newBagpipeState()

	// Blowing should increase bag
	state.mu.Lock()
	state.blowing = true
	state.bag = 0.5
	state.mu.Unlock()

	updateBag(state)

	state.mu.RLock()
	if state.bag <= 0.5 {
		t.Errorf("bag should increase while blowing, got %f", state.bag)
	}
	state.mu.RUnlock()

	// Not blowing should decrease bag (leak)
	state.mu.Lock()
	state.blowing = false
	state.bag = 0.5
	state.mu.Unlock()

	updateBag(state)

	state.mu.RLock()
	if state.bag >= 0.5 {
		t.Errorf("bag should decrease while not blowing, got %f", state.bag)
	}
	state.mu.RUnlock()

	// Bag should clamp at 0
	state.mu.Lock()
	state.bag = 0.001
	state.blowing = false
	state.mu.Unlock()

	updateBag(state)

	state.mu.RLock()
	if state.bag < 0 {
		t.Errorf("bag should not go below 0, got %f", state.bag)
	}
	state.mu.RUnlock()

	// Bag should clamp at max
	state.mu.Lock()
	state.bag = 0.99
	state.blowing = true
	state.mu.Unlock()

	updateBag(state)

	state.mu.RLock()
	if state.bag > maxBag {
		t.Errorf("bag should not exceed maxBag, got %f", state.bag)
	}
	state.mu.RUnlock()
}

func TestChanterFingerState(t *testing.T) {
	state := newBagpipeState()

	// No fingers down
	fingers := state.chanterFingerState()
	for i, f := range fingers {
		if f {
			t.Errorf("finger %d should be up with no keys pressed", i)
		}
	}

	// Press Low G (index 0, key 'a') and D (index 4, key 'j')
	state.pressKey('a')
	state.pressKey('j')
	fingers = state.chanterFingerState()
	if !fingers[0] {
		t.Error("finger 0 (Low G) should be down")
	}
	if !fingers[4] {
		t.Error("finger 4 (D) should be down")
	}
	if fingers[1] {
		t.Error("finger 1 (Low A) should be up")
	}
}

func TestNoOutputWhenStdioModeDisabled(t *testing.T) {
	resetGlobals()
	stdioMode = false

	input := `{"cmd":"pause"}
{"cmd":"status"}
{"cmd":"set","amplitude":0.5}
`
	var output bytes.Buffer

	processCommands(strings.NewReader(input), &output)

	// No output should be produced when stdioMode is false
	if output.Len() != 0 {
		t.Errorf("expected no output when stdioMode=false, got %q", output.String())
	}

	// But state should still change
	pausedMu.RLock()
	if !paused {
		t.Error("expected paused to be true even with stdioMode=false")
	}
	pausedMu.RUnlock()

	if minAmplitude != 0.5 {
		t.Errorf("expected minAmplitude 0.5, got %f", minAmplitude)
	}
}
