// spank detects slaps/hits on the laptop and runs an ASCII footrace.
// It reads the Apple Silicon accelerometer directly via IOKit HID —
// no separate sensor daemon required. Needs sudo.
//
// Slap or tap your laptop to sprint! Each hit accelerates your runner.
// Inspired by Microsoft Decathlon's alternating-key sprinting mechanic.
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
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"
	"github.com/taigrr/apple-silicon-accelerometer/detector"
	"github.com/taigrr/apple-silicon-accelerometer/sensor"
	"github.com/taigrr/apple-silicon-accelerometer/shm"
)

var version = "dev"

var (
	fastMode      bool
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

	// Physics constants
	tapImpulse     = 2.0   // m/s added per tap
	maxPlayerSpeed = 12.0  // m/s cap (Usain Bolt peaks ~12.2 m/s)
	friction       = 0.975 // velocity multiplier per frame at 30fps
	renderFPS      = 30
	trackWidth     = 60 // display characters for the track
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

// --------------------------------------------------------------------
// Runner & Race types
// --------------------------------------------------------------------

type runner struct {
	name        string
	lane        int
	position    float64 // meters
	velocity    float64 // m/s
	animFrame   int
	animCounter int
	isPlayer    bool
	finished    bool
	finishTime  time.Duration
	// AI fields
	targetSpeed float64
	accelRate   float64
	jitter      float64
}

// Running animation frames (side-view stick figure)
var runFrames = []string{
	`o/`,
	`o-`,
	`o\`,
	`o-`,
}

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

// amplitudeToBoost maps a detected slap amplitude to a speed boost
// multiplier. Harder hits give a bigger boost.
// Returns a value in [0.5, 1.5].
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

// amplitudeToVolume is kept for backward compatibility with stdio commands.
// Maps amplitude to a volume level in [-3.0, 0.0].
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

// --------------------------------------------------------------------
// Rendering
// --------------------------------------------------------------------

func clearScreen() {
	fmt.Print("\033[2J\033[H")
}

func hideCursor() {
	fmt.Print("\033[?25l")
}

func showCursor() {
	fmt.Print("\033[?25h")
}

func renderRace(runners []*runner, elapsed time.Duration, distance float64, phase string) {
	fmt.Print("\033[H") // cursor home (no clear to reduce flicker)

	var buf strings.Builder

	seconds := elapsed.Seconds()
	timeStr := fmt.Sprintf("%02d:%05.2f", int(seconds)/60, math.Mod(seconds, 60))

	// Header
	buf.WriteString("\n")
	buf.WriteString(fmt.Sprintf("  %-40s Time: %s\n", "SPANK SPRINT  "+fmt.Sprintf("%.0fm DASH", distance), timeStr))
	buf.WriteString("\n")

	// Track top border with distance markers
	border := "  " + strings.Repeat("·", trackWidth+4) + "|\n"
	buf.WriteString(border)

	finishLabel := fmt.Sprintf("%*s", trackWidth+3, "FINISH")
	buf.WriteString("  " + finishLabel + "|\n")

	// Lanes
	for _, r := range runners {
		col := int(r.position / distance * float64(trackWidth))
		if col > trackWidth {
			col = trackWidth
		}
		if col < 0 {
			col = 0
		}

		// Choose sprite
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

		// Build lane: "  1 NAME   [sprite at position]          |"
		lane := make([]byte, trackWidth+2)
		for i := range lane {
			lane[i] = ' '
		}
		lane[trackWidth+1] = '|'

		// Place sprite
		spriteBytes := []byte(sprite)
		for i, b := range spriteBytes {
			pos := col + i
			if pos < trackWidth+1 {
				lane[pos] = b
			}
		}

		buf.WriteString(fmt.Sprintf("  %d %-5s %s\n", r.lane, label, string(lane)))
	}

	// Track bottom border
	buf.WriteString(border)
	buf.WriteString("\n")

	// Status line
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
	elapsed := time.Duration(0)
	renderRace(runners, elapsed, distance, "countdown")

	// Countdown number big and centered
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

	// Sort by finish time (unfinished last)
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

// --------------------------------------------------------------------
// CLI
// --------------------------------------------------------------------

func main() {
	cmd := &cobra.Command{
		Use:   "spank",
		Short: "ASCII sprint race powered by slapping your laptop",
		Long: `spank reads the Apple Silicon accelerometer directly via IOKit HID
and runs an ASCII-animated footrace. Slap or tap the laptop to sprint!

Each detected hit accelerates your runner. Harder hits give bigger boosts.
Race against AI opponents in a 100m dash (configurable with --distance).

Requires sudo (for IOKit HID access to the accelerometer).
Inspired by Microsoft Decathlon's alternating-key sprinting mechanic.`,
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
	if raceDistance <= 0 {
		return fmt.Errorf("--distance must be greater than 0")
	}
	if numOpponents < 1 || numOpponents > 4 {
		return fmt.Errorf("--opponents must be between 1 and 4")
	}

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Create shared memory for accelerometer data.
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

	return runRace(ctx, accelRing, tuning)
}

// --------------------------------------------------------------------
// Race game loop
// --------------------------------------------------------------------

// tapEvent signals that a slap was detected with given amplitude.
type tapEvent struct {
	amplitude float64
}

func runRace(ctx context.Context, accelRing *shm.RingBuffer, tuning runtimeTuning) error {
	// Build runners
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

	// Start stdin command reader if in JSON mode
	if stdioMode {
		go readStdinCommands()
	}

	hideCursor()
	defer showCursor()
	clearScreen()

	// Countdown
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

	// Start tap detection in a goroutine that sends events
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

		// Check if paused
		pausedMu.RLock()
		isPaused := paused
		pausedMu.RUnlock()
		if isPaused {
			renderRace(runners, time.Since(startTime), raceDistance, "racing")
			continue
		}

		// Drain tap events and apply boosts to player
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

		// Update all runners
		allFinished := true
		for _, r := range runners {
			if r.finished {
				continue
			}
			allFinished = false

			if !r.isPlayer {
				// AI: gradually accelerate toward target speed with jitter
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
				// Player: apply friction decay
				r.velocity *= math.Pow(friction, float64(renderFPS)*dt)
				if r.velocity < 0.01 {
					r.velocity = 0
				}
			}

			r.position += r.velocity * dt

			// Check finish
			if r.position >= raceDistance {
				r.position = raceDistance
				r.finished = true
				r.finishTime = elapsed
				if r.isPlayer && stdioMode {
					event := map[string]interface{}{
						"event":      "finish",
						"runner":     r.name,
						"time":       elapsed.Seconds(),
						"is_player":  true,
					}
					if data, err := json.Marshal(event); err == nil {
						fmt.Println(string(data))
					}
				}
			}

			// Animate running sprite
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

		// After race finished, just keep displaying until Ctrl+C
		if raceFinished {
			// Idle loop waiting for exit
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

// detectTaps polls the accelerometer and sends tap events on the channel.
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

// --------------------------------------------------------------------
// Stdin command interface (kept for stdio/GUI integration)
// --------------------------------------------------------------------

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
