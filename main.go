// spank detects slaps/hits on the laptop and runs an ASCII footrace,
// or transforms your MacBook into a fully operational accordion.
// It reads the Apple Silicon accelerometer directly via IOKit HID —
// no separate sensor daemon required. Needs sudo.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/fang"
	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/speaker"
	"github.com/spf13/cobra"
	"github.com/taigrr/apple-silicon-accelerometer/detector"
	"github.com/taigrr/apple-silicon-accelerometer/sensor"
	"github.com/taigrr/apple-silicon-accelerometer/shm"
)

var version = "dev"

var (
	fastMode      bool
	accordionMode bool
	minAmplitude  float64
	cooldownMs    int
	stdioMode     bool
	volumeScaling bool
	paused        bool
	pausedMu      sync.RWMutex
	speedRatio    float64
	raceDistance   float64
	numOpponents  int
)

// sensorReady is closed once shared memory is created and the sensor
// worker is about to enter the CFRunLoop.
var sensorReady = make(chan struct{})

// sensorErr receives any error from the sensor worker.
var sensorErr = make(chan error, 1)

const (
	defaultMinAmplitude       = 0.05
	defaultCooldownMs         = 200 // shorter cooldown for responsive sprinting
	defaultSpeedRatio         = 1.0
	defaultSensorPollInterval = 10 * time.Millisecond
	defaultMaxSampleBatch     = 200
	sensorStartupDelay        = 100 * time.Millisecond
	defaultRaceDistance       = 100.0
	defaultNumOpponents       = 3

	// Footrace physics
	tapImpulse     = 2.0
	maxPlayerSpeed = 12.0
	friction       = 0.975
	renderFPS      = 30
	trackWidth     = 60
)

type runtimeTuning struct {
	minAmplitude float64
	cooldown     time.Duration
	pollInterval time.Duration
	maxBatch     int
}

func defaultTuning() runtimeTuning {
	return runtimeTuning{
		minAmplitude: defaultMinAmplitude,
		cooldown:     time.Duration(defaultCooldownMs) * time.Millisecond,
		pollInterval: defaultSensorPollInterval,
		maxBatch:     defaultMaxSampleBatch,
	}
}

func applyFastOverlay(base runtimeTuning) runtimeTuning {
	base.pollInterval = 4 * time.Millisecond
	base.cooldown = 150 * time.Millisecond
	if base.minAmplitude > 0.18 {
		base.minAmplitude = 0.18
	}
	if base.maxBatch < 320 {
		base.maxBatch = 320
	}
	return base
}

// ====================================================================
// Runner & Footrace types
// ====================================================================

type runner struct {
	name        string
	lane        int
	position    float64
	velocity    float64
	animFrame   int
	animCounter int
	isPlayer    bool
	finished    bool
	finishTime  time.Duration
	targetSpeed float64
	accelRate   float64
	jitter      float64
}

var runFrames = []string{`o/`, `o-`, `o\`, `o-`}

const idleFrame = `o|`
const winFrame = `\o/`

type aiProfile struct {
	name        string
	targetSpeed float64
	accelRate   float64
	jitter      float64
}

var defaultAIProfiles = []aiProfile{
	{"BOLT", 10.0, 0.7, 0.3},
	{"CARL", 9.2, 0.55, 0.4},
	{"FAYE", 8.6, 0.65, 0.25},
	{"DREW", 8.0, 0.45, 0.5},
}

func amplitudeToBoost(amplitude float64) float64 {
	const (
		minAmp   = 0.05
		maxAmp   = 0.80
		minBoost = 0.5
		maxBoost = 1.5
	)
	if amplitude <= minAmp {
		return minBoost
	}
	if amplitude >= maxAmp {
		return maxBoost
	}
	t := (amplitude - minAmp) / (maxAmp - minAmp)
	t = math.Log(1+t*99) / math.Log(100)
	return minBoost + t*(maxBoost-minBoost)
}

func amplitudeToVolume(amplitude float64) float64 {
	const (
		minAmp = 0.05
		maxAmp = 0.80
		minVol = -3.0
		maxVol = 0.0
	)
	if amplitude <= minAmp {
		return minVol
	}
	if amplitude >= maxAmp {
		return maxVol
	}
	t := (amplitude - minAmp) / (maxAmp - minAmp)
	t = math.Log(1+t*99) / math.Log(100)
	return minVol + t*(maxVol-minVol)
}

// ====================================================================
// Terminal helpers
// ====================================================================

func clearScreen() {
	fmt.Print("\033[2J\033[H")
}

func hideCursor() {
	fmt.Print("\033[?25l")
}

func showCursor() {
	fmt.Print("\033[?25h")
}

// ====================================================================
// Footrace rendering
// ====================================================================

func renderRace(runners []*runner, elapsed time.Duration, distance float64, phase string) {
	fmt.Print("\033[H")
	var buf strings.Builder

	seconds := elapsed.Seconds()
	timeStr := fmt.Sprintf("%02d:%05.2f", int(seconds)/60, math.Mod(seconds, 60))

	buf.WriteString("\n")
	buf.WriteString(fmt.Sprintf("  %-40s Time: %s\n", "SPANK SPRINT  "+fmt.Sprintf("%.0fm DASH", distance), timeStr))
	buf.WriteString("\n")

	border := "  " + strings.Repeat("·", trackWidth+4) + "|\n"
	buf.WriteString(border)

	finishLabel := fmt.Sprintf("%*s", trackWidth+3, "FINISH")
	buf.WriteString("  " + finishLabel + "|\n")

	for _, r := range runners {
		col := int(r.position / distance * float64(trackWidth))
		if col > trackWidth {
			col = trackWidth
		}
		if col < 0 {
			col = 0
		}

		var sprite string
		switch {
		case r.finished:
			sprite = winFrame
		case r.velocity < 0.3:
			sprite = idleFrame
		default:
			sprite = runFrames[r.animFrame%len(runFrames)]
		}

		label := r.name
		if r.isPlayer {
			label = r.name + "*"
		}

		lane := make([]byte, trackWidth+2)
		for i := range lane {
			lane[i] = ' '
		}
		lane[trackWidth+1] = '|'

		spriteBytes := []byte(sprite)
		for i, b := range spriteBytes {
			pos := col + i
			if pos < trackWidth+1 {
				lane[pos] = b
			}
		}

		buf.WriteString(fmt.Sprintf("  %d %-5s %s\n", r.lane, label, string(lane)))
	}

	buf.WriteString(border)
	buf.WriteString("\n")

	switch phase {
	case "countdown":
		buf.WriteString("                  GET READY...\n")
	case "racing":
		player := runners[0]
		speedStr := fmt.Sprintf("%.1f m/s", player.velocity)
		distStr := fmt.Sprintf("%.1f m / %.0f m", player.position, distance)
		buf.WriteString(fmt.Sprintf("  Speed: %-12s  Distance: %-20s  SLAP to sprint!\n", speedStr, distStr))
	case "finished":
		buf.WriteString("  RACE OVER!\n")
	}

	buf.WriteString("\n")
	fmt.Print(buf.String())
}

func renderCountdown(runners []*runner, distance float64, count int) {
	renderRace(runners, 0, distance, "countdown")

	var label string
	switch count {
	case 3:
		label = "         >>> 3 <<<"
	case 2:
		label = "         >>> 2 <<<"
	case 1:
		label = "         >>> 1 <<<"
	case 0:
		label = "        >>> GO! <<<"
	}
	fmt.Printf("\n%s\n", label)
}

func renderResults(runners []*runner) {
	fmt.Println()
	fmt.Println("  ╔══════════════════════════════════╗")
	fmt.Println("  ║          RACE  RESULTS           ║")
	fmt.Println("  ╠══════════════════════════════════╣")

	sorted := make([]*runner, len(runners))
	copy(sorted, runners)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			iTime := sorted[i].finishTime
			jTime := sorted[j].finishTime
			if !sorted[i].finished {
				iTime = time.Hour
			}
			if !sorted[j].finished {
				jTime = time.Hour
			}
			if jTime < iTime {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	places := []string{"1st", "2nd", "3rd", "4th", "5th", "6th", "7th", "8th"}
	for i, r := range sorted {
		place := places[i]
		tag := "    "
		if r.isPlayer {
			tag = " <- YOU"
		}
		timeStr := "DNF"
		if r.finished {
			secs := r.finishTime.Seconds()
			timeStr = fmt.Sprintf("%05.2fs", secs)
		}
		fmt.Printf("  ║  %s  %-6s  %s%s\n", place, r.name, timeStr, tag)
	}
	fmt.Println("  ║                                  ║")
	fmt.Println("  ╚══════════════════════════════════╝")
	fmt.Println()
}

// ====================================================================
// CLI
// ====================================================================

func main() {
	cmd := &cobra.Command{
		Use:   "spank",
		Short: "ASCII sprint race or accordion, powered by slapping your laptop",
		Long: `spank reads the Apple Silicon accelerometer directly via IOKit HID.

DEFAULT MODE (footrace):
  Slap or tap the laptop to sprint in an ASCII-animated 100m dash.
  Race against AI opponents. Harder hits give bigger boosts.

ACCORDION MODE (--accordion):
  Transforms your MacBook into a fully operational accordion.
  The hinge acts as bellows — tilt to pump air.
  The keyboard plays notes — piano-style layout.
  The accelerometer provides expression and vibrato.

Requires sudo (for IOKit HID access to the accelerometer).`,
		Version: version,
		RunE: func(cmd *cobra.Command, args []string) error {
			tuning := defaultTuning()
			if fastMode {
				tuning = applyFastOverlay(tuning)
			}
			if cmd.Flags().Changed("min-amplitude") {
				tuning.minAmplitude = minAmplitude
			}
			if cmd.Flags().Changed("cooldown") {
				tuning.cooldown = time.Duration(cooldownMs) * time.Millisecond
			}
			return run(cmd.Context(), tuning)
		},
		SilenceUsage: true,
	}

	cmd.Flags().BoolVar(&accordionMode, "accordion", false, "Accordion mode: MacBook becomes a playable accordion")
	cmd.Flags().BoolVar(&fastMode, "fast", false, "Faster polling, shorter cooldown, higher sensitivity")
	cmd.Flags().Float64Var(&minAmplitude, "min-amplitude", defaultMinAmplitude, "Minimum amplitude threshold (0.0-1.0)")
	cmd.Flags().IntVar(&cooldownMs, "cooldown", defaultCooldownMs, "Cooldown between tap detections in ms")
	cmd.Flags().BoolVar(&stdioMode, "stdio", false, "Enable stdio mode: JSON output and stdin commands")
	cmd.Flags().BoolVar(&volumeScaling, "volume-scaling", false, "Scale speed boost by slap amplitude (harder hits = bigger boost)")
	cmd.Flags().Float64Var(&speedRatio, "speed", defaultSpeedRatio, "Game speed multiplier (1.0 = normal, 2.0 = double)")
	cmd.Flags().Float64Var(&raceDistance, "distance", defaultRaceDistance, "Race distance in meters")
	cmd.Flags().IntVar(&numOpponents, "opponents", defaultNumOpponents, "Number of AI opponents (1-4)")

	if err := fang.Execute(context.Background(), cmd); err != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context, tuning runtimeTuning) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("spank requires root privileges for accelerometer access, run with: sudo spank")
	}

	if tuning.minAmplitude < 0 || tuning.minAmplitude > 1 {
		return fmt.Errorf("--min-amplitude must be between 0.0 and 1.0")
	}
	if tuning.cooldown <= 0 {
		return fmt.Errorf("--cooldown must be greater than 0")
	}

	if !accordionMode {
		if raceDistance <= 0 {
			return fmt.Errorf("--distance must be greater than 0")
		}
		if numOpponents < 1 || numOpponents > 4 {
			return fmt.Errorf("--opponents must be between 1 and 4")
		}
	}

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	accelRing, err := shm.CreateRing(shm.NameAccel)
	if err != nil {
		return fmt.Errorf("creating accel shm: %w", err)
	}
	defer accelRing.Close()
	defer accelRing.Unlink()

	go func() {
		close(sensorReady)
		if err := sensor.Run(sensor.Config{
			AccelRing: accelRing,
			Restarts:  0,
		}); err != nil {
			sensorErr <- err
		}
	}()

	select {
	case <-sensorReady:
	case err := <-sensorErr:
		return fmt.Errorf("sensor worker failed: %w", err)
	case <-ctx.Done():
		return nil
	}

	time.Sleep(sensorStartupDelay)

	if accordionMode {
		return runAccordion(ctx, accelRing, tuning)
	}
	return runRace(ctx, accelRing, tuning)
}

// ====================================================================
// Footrace game loop
// ====================================================================

type tapEvent struct {
	amplitude float64
}

func runRace(ctx context.Context, accelRing *shm.RingBuffer, tuning runtimeTuning) error {
	runners := make([]*runner, 0, numOpponents+1)
	runners = append(runners, &runner{
		name:     "YOU",
		lane:     1,
		isPlayer: true,
	})
	for i := 0; i < numOpponents && i < len(defaultAIProfiles); i++ {
		ai := defaultAIProfiles[i]
		runners = append(runners, &runner{
			name:        ai.name,
			lane:        i + 2,
			targetSpeed: ai.targetSpeed * speedRatio,
			accelRate:   ai.accelRate,
			jitter:      ai.jitter,
		})
	}

	if stdioMode {
		go readStdinCommands()
	}

	hideCursor()
	defer showCursor()
	clearScreen()

	for count := 3; count >= 0; count-- {
		select {
		case <-ctx.Done():
			showCursor()
			fmt.Println("\nbye!")
			return nil
		default:
		}
		renderCountdown(runners, raceDistance, count)
		if count > 0 {
			time.Sleep(1 * time.Second)
		} else {
			time.Sleep(400 * time.Millisecond)
		}
	}

	tapCh := make(chan tapEvent, 32)
	go detectTaps(ctx, accelRing, tuning, tapCh)

	startTime := time.Now()
	frameTicker := time.NewTicker(time.Second / renderFPS)
	defer frameTicker.Stop()

	raceFinished := false

	for {
		select {
		case <-ctx.Done():
			showCursor()
			clearScreen()
			fmt.Println("\nRace cancelled. bye!")
			return nil
		case <-frameTicker.C:
		}

		pausedMu.RLock()
		isPaused := paused
		pausedMu.RUnlock()
		if isPaused {
			renderRace(runners, time.Since(startTime), raceDistance, "racing")
			continue
		}

		player := runners[0]
	drainTaps:
		for {
			select {
			case ev := <-tapCh:
				if !player.finished {
					boost := tapImpulse * speedRatio
					if volumeScaling {
						boost *= amplitudeToBoost(ev.amplitude)
					}
					player.velocity += boost
					if player.velocity > maxPlayerSpeed*speedRatio {
						player.velocity = maxPlayerSpeed * speedRatio
					}
				}
			default:
				break drainTaps
			}
		}

		elapsed := time.Since(startTime)
		dt := 1.0 / float64(renderFPS)

		allFinished := true
		for _, r := range runners {
			if r.finished {
				continue
			}
			allFinished = false

			if !r.isPlayer {
				diff := r.targetSpeed - r.velocity
				r.velocity += diff * r.accelRate * dt
				r.velocity += (rand.Float64()*2 - 1) * r.jitter * dt
				if r.velocity < 0 {
					r.velocity = 0
				}
				if r.velocity > r.targetSpeed*1.05 {
					r.velocity = r.targetSpeed * 1.05
				}
			} else {
				r.velocity *= math.Pow(friction, float64(renderFPS)*dt)
				if r.velocity < 0.01 {
					r.velocity = 0
				}
			}

			r.position += r.velocity * dt

			if r.position >= raceDistance {
				r.position = raceDistance
				r.finished = true
				r.finishTime = elapsed
				if r.isPlayer && stdioMode {
					event := map[string]interface{}{
						"event":     "finish",
						"runner":    r.name,
						"time":      elapsed.Seconds(),
						"is_player": true,
					}
					if data, err := json.Marshal(event); err == nil {
						fmt.Println(string(data))
					}
				}
			}

			if r.velocity > 0.3 {
				r.animCounter++
				framesPerStep := max(1, int(8.0/(r.velocity+0.1)))
				if r.animCounter >= framesPerStep {
					r.animFrame = (r.animFrame + 1) % len(runFrames)
					r.animCounter = 0
				}
			}
		}

		phase := "racing"
		if allFinished || raceFinished {
			phase = "finished"
		}

		renderRace(runners, elapsed, raceDistance, phase)

		if allFinished && !raceFinished {
			raceFinished = true
			renderResults(runners)
			fmt.Println("  Press Ctrl+C to exit.")

			if stdioMode {
				results := make([]map[string]interface{}, len(runners))
				for i, r := range runners {
					results[i] = map[string]interface{}{
						"runner":    r.name,
						"time":      r.finishTime.Seconds(),
						"is_player": r.isPlayer,
					}
				}
				if data, err := json.Marshal(map[string]interface{}{
					"event":   "race_complete",
					"results": results,
				}); err == nil {
					fmt.Println(string(data))
				}
			}
		}

		if raceFinished {
			select {
			case <-ctx.Done():
				showCursor()
				fmt.Println("\nbye!")
				return nil
			case <-frameTicker.C:
				continue
			}
		}
	}
}

func detectTaps(ctx context.Context, accelRing *shm.RingBuffer, tuning runtimeTuning, tapCh chan<- tapEvent) {
	det := detector.New()
	var lastAccelTotal uint64
	var lastEventTime time.Time
	var lastTap time.Time

	ticker := time.NewTicker(tuning.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		pausedMu.RLock()
		isPaused := paused
		pausedMu.RUnlock()
		if isPaused {
			continue
		}

		now := time.Now()
		tNow := float64(now.UnixNano()) / 1e9

		samples, newTotal := accelRing.ReadNew(lastAccelTotal, shm.AccelScale)
		lastAccelTotal = newTotal
		if len(samples) > tuning.maxBatch {
			samples = samples[len(samples)-tuning.maxBatch:]
		}

		nSamples := len(samples)
		for idx, sample := range samples {
			tSample := tNow - float64(nSamples-idx-1)/float64(det.FS)
			det.Process(sample.X, sample.Y, sample.Z, tSample)
		}

		if len(det.Events) == 0 {
			continue
		}

		ev := det.Events[len(det.Events)-1]
		if ev.Time.Equal(lastEventTime) {
			continue
		}
		lastEventTime = ev.Time

		if time.Since(lastTap) <= tuning.cooldown {
			continue
		}
		if ev.Amplitude < tuning.minAmplitude {
			continue
		}

		lastTap = now

		select {
		case tapCh <- tapEvent{amplitude: ev.Amplitude}:
		default:
		}
	}
}

// ====================================================================
// Accordion mode
// ====================================================================

const (
	accordionSampleRate = 44100
	accordionFPS        = 30

	// Bellows physics
	bellowsDecay    = 0.92 // how fast bellows pressure fades per frame
	bellowsGain     = 3.0  // amplifier for detected motion
	maxBellows      = 1.0  // maximum bellows pressure
	vibratoRate     = 5.5  // Hz
	vibratoDepth    = 0.02 // frequency modulation depth

	// Key repeat timeout — if a key hasn't been re-pressed within this
	// duration, consider it released (terminal key repeat is ~30-60ms).
	keyHoldTimeout = 180 * time.Millisecond
)

// noteFreq returns the frequency in Hz for a given MIDI note number.
// Middle C (C4) = MIDI 60 = 261.63 Hz.
func noteFreq(midi int) float64 {
	return 440.0 * math.Pow(2.0, float64(midi-69)/12.0)
}

// noteName returns a human-readable name for a MIDI note.
func noteName(midi int) string {
	names := []string{"C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"}
	octave := (midi / 12) - 1
	note := midi % 12
	return fmt.Sprintf("%s%d", names[note], octave)
}

// keyToMIDI maps keyboard characters to MIDI note numbers.
// Layout mimics a piano keyboard across two rows:
//
//	Black keys:  2  3     5  6  7     9  0
//	             C# D#    F# G# A#   C# D#
//	White keys: Q  W  E  R  T  Y  U  I  O  P
//	            C4 D4 E4 F4 G4 A4 B4 C5 D5 E5
//
//	Black keys:  s  d     g  h  j     l  ;
//	             C# D#    F# G# A#   C# D#
//	White keys: Z  X  C  V  B  N  M  ,  .  /
//	            C3 D3 E3 F3 G3 A3 B3 C4 D4 E4
var keyToMIDI = map[byte]int{
	// Upper octave — white keys (C4-E5)
	'q': 60, 'w': 62, 'e': 64, 'r': 65, 't': 67, 'y': 69, 'u': 71,
	'i': 72, 'o': 74, 'p': 76,
	// Upper octave — black keys
	'2': 61, '3': 63, '5': 66, '6': 68, '7': 70,
	'9': 73, '0': 75,
	// Lower octave — white keys (C3-E4)
	'z': 48, 'x': 50, 'c': 52, 'v': 53, 'b': 55, 'n': 57, 'm': 59,
	',': 60, '.': 62, '/': 64,
	// Lower octave — black keys
	's': 49, 'd': 51, 'g': 54, 'h': 56, 'j': 58,
	'l': 61, ';': 63,
}

// accordionState holds the live state of the accordion instrument.
type accordionState struct {
	mu            sync.RWMutex
	activeKeys    map[byte]time.Time // key -> last press time
	bellows       float64            // 0.0 (silent) to 1.0 (full volume)
	tiltX         float64            // lateral tilt for vibrato
	tiltY         float64            // forward/back tilt — main bellows driver
	prevTiltY     float64            // previous frame tilt for delta
	speakerInited bool
}

func newAccordionState() *accordionState {
	return &accordionState{
		activeKeys: make(map[byte]time.Time),
	}
}

// activeNotes returns the MIDI notes currently being held, sorted.
func (a *accordionState) activeNotes() []int {
	a.mu.RLock()
	defer a.mu.RUnlock()

	now := time.Now()
	notes := make([]int, 0, len(a.activeKeys))
	for key, t := range a.activeKeys {
		if now.Sub(t) <= keyHoldTimeout {
			if midi, ok := keyToMIDI[key]; ok {
				notes = append(notes, midi)
			}
		}
	}
	// Sort for deterministic display
	for i := 0; i < len(notes); i++ {
		for j := i + 1; j < len(notes); j++ {
			if notes[j] < notes[i] {
				notes[i], notes[j] = notes[j], notes[i]
			}
		}
	}
	return notes
}

// expireKeys removes keys that haven't been re-pressed recently.
func (a *accordionState) expireKeys() {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for key, t := range a.activeKeys {
		if now.Sub(t) > keyHoldTimeout {
			delete(a.activeKeys, key)
		}
	}
}

// pressKey registers a keypress.
func (a *accordionState) pressKey(key byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activeKeys[key] = time.Now()
}

// toneGenerator streams a sine wave for the accordion, mixing all active
// notes and modulating by bellows pressure and vibrato.
type toneGenerator struct {
	state      *accordionState
	sampleRate float64
	pos        uint64
}

func (t *toneGenerator) Stream(samples [][2]float64) (int, bool) {
	t.state.mu.RLock()
	bellows := t.state.bellows
	tiltX := t.state.tiltX
	t.state.mu.RUnlock()

	activeNotes := t.state.activeNotes()

	for i := range samples {
		if len(activeNotes) == 0 || bellows < 0.01 {
			samples[i] = [2]float64{0, 0}
			t.pos++
			continue
		}

		tSec := float64(t.pos) / t.sampleRate

		// Vibrato from lateral tilt
		vibrato := 1.0 + tiltX*vibratoDepth*math.Sin(2*math.Pi*vibratoRate*tSec)

		// Mix all active notes
		val := 0.0
		for _, midi := range activeNotes {
			freq := noteFreq(midi) * vibrato
			phase := 2 * math.Pi * freq * tSec

			// Accordion timbre: fundamental + odd harmonics (reed-like)
			v := math.Sin(phase)                    // fundamental
			v += 0.5 * math.Sin(3*phase)            // 3rd harmonic
			v += 0.25 * math.Sin(5*phase)           // 5th harmonic
			v += 0.12 * math.Sin(7*phase)           // 7th harmonic
			v += 0.06 * math.Sin(9*phase)           // 9th harmonic
			val += v * 0.4 // scale per-note volume
		}

		// Normalize if many notes
		if len(activeNotes) > 1 {
			val /= math.Sqrt(float64(len(activeNotes)))
		}

		// Apply bellows volume envelope
		val *= bellows

		// Soft clamp
		if val > 0.95 {
			val = 0.95
		}
		if val < -0.95 {
			val = -0.95
		}

		samples[i] = [2]float64{val, val}
		t.pos++
	}
	return len(samples), true
}

func (t *toneGenerator) Err() error { return nil }

// enableRawTerminal puts the terminal in raw mode for single-keypress input.
func enableRawTerminal() error {
	// Use stty on macOS — the only target platform
	cmd := exec.Command("stty", "raw", "-echo")
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// restoreTerminal restores normal terminal mode.
func restoreTerminal() {
	cmd := exec.Command("stty", "sane")
	cmd.Stdin = os.Stdin
	_ = cmd.Run()
}

// readKeys reads raw keypresses from stdin and sends them on keyCh.
func readKeys(ctx context.Context, keyCh chan<- byte) {
	buf := make([]byte, 1)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			continue
		}
		// Ctrl+C detection (raw mode doesn't generate SIGINT)
		if buf[0] == 3 {
			// Send ourselves SIGINT to trigger clean shutdown
			syscall.Kill(syscall.Getpid(), syscall.SIGINT)
			return
		}
		select {
		case keyCh <- buf[0]:
		default:
		}
	}
}

// readBellows polls the accelerometer and updates bellows state.
func readBellows(ctx context.Context, accelRing *shm.RingBuffer, state *accordionState) {
	var lastAccelTotal uint64

	ticker := time.NewTicker(defaultSensorPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		samples, newTotal := accelRing.ReadNew(lastAccelTotal, shm.AccelScale)
		lastAccelTotal = newTotal

		if len(samples) == 0 {
			continue
		}

		// Use the latest sample's gravity vector to estimate tilt.
		// On a MacBook sitting flat: X≈0, Y≈0, Z≈-1g
		// Tilting forward/back changes Y; tilting sideways changes X.
		latest := samples[len(samples)-1]

		state.mu.Lock()
		state.tiltX = latest.X
		newTiltY := latest.Y

		// Bellows pressure = rate of change of tilt (pumping motion).
		// Opening/closing the lid or rocking the laptop drives this.
		delta := math.Abs(newTiltY - state.prevTiltY)
		state.prevTiltY = newTiltY
		state.tiltY = newTiltY

		// Also factor in overall motion magnitude (shaking/squeezing)
		accelMag := math.Sqrt(latest.X*latest.X+latest.Y*latest.Y+latest.Z*latest.Z) - 1.0
		if accelMag < 0 {
			accelMag = 0
		}

		// Drive bellows from tilt delta + general motion
		state.bellows += (delta + accelMag*0.5) * bellowsGain
		if state.bellows > maxBellows {
			state.bellows = maxBellows
		}

		// Decay
		state.bellows *= bellowsDecay
		if state.bellows < 0.01 {
			state.bellows = 0
		}
		state.mu.Unlock()
	}
}

// renderAccordion draws the ASCII accordion interface.
func renderAccordion(state *accordionState) {
	fmt.Print("\033[H") // cursor home

	state.mu.RLock()
	bellows := state.bellows
	tiltY := state.tiltY
	state.mu.RUnlock()

	activeNotes := state.activeNotes()

	var buf strings.Builder

	// Header
	buf.WriteString("\n")
	buf.WriteString("  ╔══════════════════════════════════════════════════════════════╗\n")
	buf.WriteString("  ║           SPANK ACCORDION  —  MacBook Squeezebox            ║\n")
	buf.WriteString("  ╠══════════════════════════════════════════════════════════════╣\n")

	// Bellows width varies with tilt and pressure
	bellowsWidth := 3 + int(math.Abs(tiltY)*8)
	if bellowsWidth > 16 {
		bellowsWidth = 16
	}
	if bellowsWidth < 3 {
		bellowsWidth = 3
	}

	// Generate bellows fill pattern
	bellowsFill := func(w int) string {
		if w <= 0 {
			return ""
		}
		pattern := make([]byte, w)
		for i := range pattern {
			if i%2 == 0 {
				pattern[i] = '>'
			} else {
				pattern[i] = '<'
			}
		}
		return string(pattern)
	}

	// Left side: bass buttons  |  bellows  |  Right side: treble buttons
	leftWidth := 12
	rightWidth := 12

	// Draw the accordion body
	padBellows := func(w int) string {
		fill := bellowsFill(w)
		return fmt.Sprintf("%-16s", fill)
	}

	// Determine active key display for left (lower octave) and right (upper octave)
	leftActive := make(map[byte]bool)
	rightActive := make(map[byte]bool)

	state.mu.RLock()
	now := time.Now()
	for key, t := range state.activeKeys {
		if now.Sub(t) <= keyHoldTimeout {
			switch key {
			case 'z', 'x', 'c', 'v', 'b', 'n', 'm', ',', '.', '/',
				's', 'd', 'g', 'h', 'j', 'l', ';':
				leftActive[key] = true
			default:
				rightActive[key] = true
			}
		}
	}
	state.mu.RUnlock()

	// Bass side buttons (lower octave)
	bassRow1 := highlightKeys("Z X C V B N M , . /", leftActive)
	bassRow2 := highlightKeys("  s d   g h j   l ;", leftActive)

	// Treble side buttons (upper octave)
	trebRow1 := highlightKeys("Q W E R T Y U I O P", rightActive)
	trebRow2 := highlightKeys("  2 3   5 6 7   9 0", rightActive)

	bf := padBellows(bellowsWidth)

	buf.WriteString(fmt.Sprintf("  ║  ┌──────────┐ %s ┌──────────┐  ║\n", bf))
	buf.WriteString(fmt.Sprintf("  ║  │%-*s│ %s │%-*s│  ║\n", leftWidth, trebRow2, bf, rightWidth, bassRow2))
	buf.WriteString(fmt.Sprintf("  ║  │%-*s│ %s │%-*s│  ║\n", leftWidth, trebRow1, bf, rightWidth, bassRow1))

	// Bellows pressure bar
	pressBar := int(bellows * 16)
	if pressBar > 16 {
		pressBar = 16
	}
	bellowsBar := strings.Repeat("█", pressBar) + strings.Repeat("░", 16-pressBar)
	buf.WriteString(fmt.Sprintf("  ║  │  TREBLE   │ %s │   BASS    │  ║\n", bf))
	buf.WriteString(fmt.Sprintf("  ║  └──────────┘ %s └──────────┘  ║\n", bf))

	buf.WriteString("  ╠══════════════════════════════════════════════════════════════╣\n")

	// Playing notes display
	noteStr := "(silence)"
	if len(activeNotes) > 0 {
		names := make([]string, len(activeNotes))
		for i, n := range activeNotes {
			names[i] = noteName(n)
		}
		noteStr = strings.Join(names, " ")
	}
	buf.WriteString(fmt.Sprintf("  ║  Notes: %-52s║\n", noteStr))

	// Bellows meter
	buf.WriteString(fmt.Sprintf("  ║  Bellows: [%s] %3.0f%%                            ║\n",
		bellowsBar, bellows*100))

	// Tilt indicators
	tiltBar := renderTiltBar(tiltY)
	buf.WriteString(fmt.Sprintf("  ║  Tilt:   [%s]                                     ║\n", tiltBar))

	buf.WriteString("  ╠══════════════════════════════════════════════════════════════╣\n")

	// Keyboard map reference
	buf.WriteString("  ║  KEYBOARD MAP (piano-style):                                ║\n")
	buf.WriteString("  ║                                                              ║\n")
	buf.WriteString("  ║  Treble:  ┌─┬─┬─┬─┬─┬─┬─┬─┬─┬─┐                            ║\n")
	buf.WriteString("  ║  Black:   │2│3│ │5│6│7│ │9│0│ │   C#D#  F#G#A#  C#D#        ║\n")
	buf.WriteString("  ║  White:   │Q│W│E│R│T│Y│U│I│O│P│   C D E F G A B C D E       ║\n")
	buf.WriteString("  ║          └─┴─┴─┴─┴─┴─┴─┴─┴─┴─┘   (octave 4-5)             ║\n")
	buf.WriteString("  ║                                                              ║\n")
	buf.WriteString("  ║  Bass:    ┌─┬─┬─┬─┬─┬─┬─┬─┬─┬─┐                            ║\n")
	buf.WriteString("  ║  Black:   │s│d│ │g│h│j│ │l│;│ │   C#D#  F#G#A#  C#D#        ║\n")
	buf.WriteString("  ║  White:   │Z│X│C│V│B│N│M│,│.│/│   C D E F G A B C D E       ║\n")
	buf.WriteString("  ║          └─┴─┴─┴─┴─┴─┴─┴─┴─┴─┘   (octave 3-4)             ║\n")
	buf.WriteString("  ║                                                              ║\n")
	buf.WriteString("  ║  TILT laptop to pump bellows  •  Ctrl+C to quit              ║\n")
	buf.WriteString("  ╚══════════════════════════════════════════════════════════════╝\n")

	fmt.Print(buf.String())
}

// highlightKeys returns the key display string with active keys uppercased/marked.
func highlightKeys(layout string, active map[byte]bool) string {
	result := make([]byte, len(layout))
	for i := 0; i < len(layout); i++ {
		ch := layout[i]
		lower := ch
		if ch >= 'A' && ch <= 'Z' {
			lower = ch + 32
		}
		if active[lower] || active[ch] {
			if ch >= 'a' && ch <= 'z' {
				result[i] = ch - 32 // uppercase to highlight
			} else if ch == ' ' {
				result[i] = ' '
			} else {
				result[i] = ch
			}
		} else {
			if ch >= 'A' && ch <= 'Z' {
				result[i] = ch + 32 // show as lowercase when not active
			} else {
				result[i] = ch
			}
		}
	}
	return string(result)
}

// renderTiltBar shows a centered tilt indicator.
func renderTiltBar(tilt float64) string {
	const width = 16
	center := width / 2
	bar := make([]byte, width)
	for i := range bar {
		bar[i] = '-'
	}
	bar[center] = '|'

	pos := center + int(tilt*float64(center))
	if pos < 0 {
		pos = 0
	}
	if pos >= width {
		pos = width - 1
	}
	bar[pos] = '#'

	return string(bar)
}

func runAccordion(ctx context.Context, accelRing *shm.RingBuffer, tuning runtimeTuning) error {
	state := newAccordionState()

	// Initialize speaker for tone generation
	sr := beep.SampleRate(accordionSampleRate)
	speaker.Init(sr, sr.N(time.Second/30))

	gen := &toneGenerator{
		state:      state,
		sampleRate: float64(accordionSampleRate),
	}

	// Start continuous tone playback (it's silent when no keys are pressed
	// or bellows are empty)
	speaker.Play(gen)

	// Put terminal in raw mode for keypress capture
	if err := enableRawTerminal(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not enable raw mode: %v\n", err)
	}
	defer restoreTerminal()

	hideCursor()
	defer showCursor()
	clearScreen()

	// Start keyboard reader
	keyCh := make(chan byte, 64)
	go readKeys(ctx, keyCh)

	// Start bellows reader (accelerometer -> bellows pressure)
	go readBellows(ctx, accelRing, state)

	// Start stdin command reader if in JSON mode
	if stdioMode {
		go readStdinCommands()
	}

	frameTicker := time.NewTicker(time.Second / accordionFPS)
	defer frameTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			speaker.Clear()
			restoreTerminal()
			showCursor()
			clearScreen()
			fmt.Println("bye!")
			return nil
		case key := <-keyCh:
			state.pressKey(key)
		case <-frameTicker.C:
			state.expireKeys()
			renderAccordion(state)
		}
	}
}

// ====================================================================
// Stdin command interface (kept for stdio/GUI integration)
// ====================================================================

type stdinCommand struct {
	Cmd       string  `json:"cmd"`
	Amplitude float64 `json:"amplitude,omitempty"`
	Cooldown  int     `json:"cooldown,omitempty"`
	Speed     float64 `json:"speed,omitempty"`
}

func readStdinCommands() {
	processCommands(os.Stdin, os.Stdout)
}

func processCommands(r io.Reader, w io.Writer) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var cmd stdinCommand
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			if stdioMode {
				fmt.Fprintf(w, `{"error":"invalid command: %s"}%s`, err.Error(), "\n")
			}
			continue
		}

		switch cmd.Cmd {
		case "pause":
			pausedMu.Lock()
			paused = true
			pausedMu.Unlock()
			if stdioMode {
				fmt.Fprintln(w, `{"status":"paused"}`)
			}
		case "resume":
			pausedMu.Lock()
			paused = false
			pausedMu.Unlock()
			if stdioMode {
				fmt.Fprintln(w, `{"status":"resumed"}`)
			}
		case "set":
			if cmd.Amplitude > 0 && cmd.Amplitude <= 1 {
				minAmplitude = cmd.Amplitude
			}
			if cmd.Cooldown > 0 {
				cooldownMs = cmd.Cooldown
			}
			if cmd.Speed > 0 {
				speedRatio = cmd.Speed
			}
			if stdioMode {
				fmt.Fprintf(w, `{"status":"settings_updated","amplitude":%.4f,"cooldown":%d,"speed":%.2f}%s`, minAmplitude, cooldownMs, speedRatio, "\n")
			}
		case "volume-scaling":
			volumeScaling = !volumeScaling
			if stdioMode {
				fmt.Fprintf(w, `{"status":"volume_scaling_toggled","volume_scaling":%t}%s`, volumeScaling, "\n")
			}
		case "status":
			pausedMu.RLock()
			isPaused := paused
			pausedMu.RUnlock()
			if stdioMode {
				fmt.Fprintf(w, `{"status":"ok","paused":%t,"amplitude":%.4f,"cooldown":%d,"volume_scaling":%t,"speed":%.2f}%s`, isPaused, minAmplitude, cooldownMs, volumeScaling, speedRatio, "\n")
			}
		default:
			if stdioMode {
				fmt.Fprintf(w, `{"error":"unknown command: %s"}%s`, cmd.Cmd, "\n")
			}
		}
	}
}
