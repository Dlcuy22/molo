package dsp

import (
	"math"
	"testing"
)

// TestEnvelopeStepResponseAttackTimeConstant verifies that the step response
// reaches approximately 63.2% of unity after one attack time constant.
func TestEnvelopeStepResponseAttackTimeConstant(t *testing.T) {
	const rate = 48000
	const attackSec = 0.01

	peakEnv := newEnvState(attackSec, 0.1, rate, envPeak)
	samples := int(attackSec * float64(rate))

	var peakVal float64
	for i := 0; i < samples; i++ {
		peakVal = peakEnv.step(1.0)
	}

	wantPeak := 1.0 - math.Exp(-1.0)
	if math.Abs(peakVal-wantPeak) > 0.005 {
		t.Fatalf("peak envelope after one attack time constant = %v, want ~%v", peakVal, wantPeak)
	}

	rmsEnv := newEnvState(attackSec, 0.1, rate, envRMS)
	var rmsVal float64
	for i := 0; i < samples; i++ {
		rmsVal = rmsEnv.step(1.0)
	}

	wantRMSPower := 1.0 - math.Exp(-1.0)
	wantRMSLevel := math.Sqrt(wantRMSPower)
	if math.Abs(rmsVal-wantRMSLevel) > 0.005 {
		t.Fatalf("RMS envelope level after one attack time constant = %v, want ~%v", rmsVal, wantRMSLevel)
	}
	if math.Abs(rmsEnv.env-wantRMSPower) > 0.005 {
		t.Fatalf("RMS power state after one attack time constant = %v, want ~%v", rmsEnv.env, wantRMSPower)
	}
}

// TestEnvelopeStepResponseReleaseTimeConstant verifies that envelope decay
// falls by approximately 63.2% after one release time constant.
func TestEnvelopeStepResponseReleaseTimeConstant(t *testing.T) {
	const rate = 48000
	const releaseSec = 0.02

	env := newEnvState(0.001, releaseSec, rate, envPeak)
	for i := 0; i < 2000; i++ {
		env.step(1.0)
	}

	samples := int(releaseSec * float64(rate))
	var val float64
	for i := 0; i < samples; i++ {
		val = env.step(0.0)
	}

	want := math.Exp(-1.0)
	if math.Abs(val-want) > 0.005 {
		t.Fatalf("envelope after one release time constant = %v, want ~%v", val, want)
	}
}

// TestEnvelopeReleaseSlowerWhenReleaseGreaterThanAttack checks that release
// decays more slowly than attack rises when releaseSec exceeds attackSec.
func TestEnvelopeReleaseSlowerWhenReleaseGreaterThanAttack(t *testing.T) {
	const rate = 48000
	env := newEnvState(0.005, 0.050, rate, envPeak)

	if env.release >= env.attack {
		t.Fatalf("release coefficient %v should be less than attack %v", env.release, env.attack)
	}

	const steps = 100
	for i := 0; i < steps; i++ {
		env.step(1.0)
	}
	risen := env.env

	for i := 0; i < 5000; i++ {
		env.step(1.0)
	}

	for i := 0; i < steps; i++ {
		env.step(0.0)
	}
	fallen := 1.0 - env.env

	if fallen >= risen {
		t.Fatalf("decay amount %v should be less than rise amount %v over %d steps", fallen, risen, steps)
	}
}

// TestEnvelopePeakRectifiesSignal verifies that peak mode tracks rectified amplitude.
func TestEnvelopePeakRectifiesSignal(t *testing.T) {
	env := newEnvState(0, 0, 48000, envPeak)

	if got := env.step(-0.75); got != 0.75 {
		t.Fatalf("step(-0.75) = %v, want 0.75", got)
	}

	frame := []float64{0.2, -0.9, 0.5, -0.3}
	if got := env.stepFrame(frame); got != 0.9 {
		t.Fatalf("stepFrame() = %v, want 0.9", got)
	}
}

// TestEnvelopeRMSTracksMeanPower verifies that RMS mode tracks root-mean-square power.
func TestEnvelopeRMSTracksMeanPower(t *testing.T) {
	env := newEnvState(0, 0, 48000, envRMS)

	gotSingle := env.step(-0.8)
	if math.Abs(gotSingle-0.8) > 1e-9 {
		t.Fatalf("step(-0.8) = %v, want 0.8", gotSingle)
	}
	if math.Abs(env.env-0.64) > 1e-9 {
		t.Fatalf("RMS internal power = %v, want 0.64", env.env)
	}

	frame := []float64{0.6, -0.8}
	wantRMS := math.Sqrt((0.36 + 0.64) / 2.0)
	if got := env.stepFrame(frame); math.Abs(got-wantRMS) > 1e-9 {
		t.Fatalf("stepFrame() = %v, want %v", got, wantRMS)
	}
	if math.Abs(env.env-0.5) > 1e-9 {
		t.Fatalf("RMS frame internal power = %v, want 0.5", env.env)
	}

	pulseFrame := []float64{2.0, 0.0, 0.0, 0.0}
	peakEnv := newEnvState(0, 0, 48000, envPeak)
	rmsEnv := newEnvState(0, 0, 48000, envRMS)

	gotPeak := peakEnv.stepFrame(pulseFrame)
	gotPulseRMS := rmsEnv.stepFrame(pulseFrame)
	if gotPeak != 2.0 {
		t.Fatalf("peakFrame = %v, want 2.0", gotPeak)
	}
	if gotPulseRMS != 1.0 {
		t.Fatalf("rmsFrame = %v, want 1.0", gotPulseRMS)
	}
}

// TestEnvelopeNonFiniteInputDoesNotPoison verifies non-finite inputs are treated as zero.
func TestEnvelopeNonFiniteInputDoesNotPoison(t *testing.T) {
	env := newEnvState(0.01, 0.01, 48000, envPeak)
	env.step(0.5)

	env.step(math.NaN())
	if math.IsNaN(env.env) || math.IsInf(env.env, 0) {
		t.Fatalf("envelope became non-finite after NaN: %v", env.env)
	}

	env.step(math.Inf(1))
	if math.IsNaN(env.env) || math.IsInf(env.env, 0) {
		t.Fatalf("envelope became non-finite after +Inf: %v", env.env)
	}

	env.step(math.Inf(-1))
	if math.IsNaN(env.env) || math.IsInf(env.env, 0) {
		t.Fatalf("envelope became non-finite after -Inf: %v", env.env)
	}

	val := env.step(1.0)
	if val <= 0 || math.IsNaN(val) || math.IsInf(val, 0) {
		t.Fatalf("recovery step failed: %v", val)
	}

	frame := []float64{math.NaN(), math.Inf(1), -0.75, math.Inf(-1)}
	frameEnv := newEnvState(0, 0, 48000, envPeak)
	if got := frameEnv.stepFrame(frame); got != 0.75 {
		t.Fatalf("frame with non-finite values = %v, want 0.75", got)
	}

	rmsEnv := newEnvState(0, 0, 48000, envRMS)
	rmsFrame := []float64{math.NaN(), math.Inf(1), 0.8, 0.0}
	wantRMS := math.Sqrt((0.0 + 0.0 + 0.64 + 0.0) / 4.0)
	if got := rmsEnv.stepFrame(rmsFrame); math.Abs(got-wantRMS) > 1e-9 {
		t.Fatalf("RMS frame with non-finite values = %v, want %v", got, wantRMS)
	}
}

// TestEnvelopeSetAttackReleaseLeavesEnvelopeUntouched verifies parameter updates
// recompute coefficients without resetting the running envelope level.
func TestEnvelopeSetAttackReleaseLeavesEnvelopeUntouched(t *testing.T) {
	const rate = 48000
	env := newEnvState(0.01, 0.02, rate, envPeak)

	env.step(0.75)
	levelBefore := env.env
	if levelBefore == 0 {
		t.Fatal("expected non-zero envelope before parameter update")
	}

	attackBefore := env.attack
	releaseBefore := env.release

	env.setAttackRelease(0.05, 0.10, rate)

	if env.env != levelBefore {
		t.Fatalf("envelope changed across setAttackRelease: got %v, want %v", env.env, levelBefore)
	}
	if env.attack == attackBefore {
		t.Fatalf("attack coefficient did not change: %v", env.attack)
	}
	if env.release == releaseBefore {
		t.Fatalf("release coefficient did not change: %v", env.release)
	}

	env.setAttackRelease(0.05, 0.10, 0)
	wantAttack := envCoeff(0.05, 48000)
	if env.attack != wantAttack {
		t.Fatalf("attack with rate 0 = %v, want %v", env.attack, wantAttack)
	}
}

// TestEnvelopeZeroAllocations verifies that step and stepFrame make zero heap allocations.
func TestEnvelopeZeroAllocations(t *testing.T) {
	env := newEnvState(0.01, 0.02, 48000, envPeak)

	allocsStep := testing.AllocsPerRun(1000, func() {
		env.step(0.5)
	})
	if allocsStep != 0 {
		t.Fatalf("step allocated %v times, want 0", allocsStep)
	}

	frame := make([]float64, 256)
	for i := range frame {
		frame[i] = 0.5
	}

	allocsFrame := testing.AllocsPerRun(1000, func() {
		env.stepFrame(frame)
	})
	if allocsFrame != 0 {
		t.Fatalf("stepFrame allocated %v times, want 0", allocsFrame)
	}

	rmsEnv := newEnvState(0.01, 0.02, 48000, envRMS)
	allocsRMS := testing.AllocsPerRun(1000, func() {
		rmsEnv.stepFrame(frame)
	})
	if allocsRMS != 0 {
		t.Fatalf("RMS stepFrame allocated %v times, want 0", allocsRMS)
	}

	allocsRMSStep := testing.AllocsPerRun(1000, func() {
		rmsEnv.step(0.5)
	})
	if allocsRMSStep != 0 {
		t.Fatalf("RMS step allocated %v times, want 0", allocsRMSStep)
	}
}

// TestEnvelopeReset verifies that reset clears tracked envelope state to zero.
func TestEnvelopeReset(t *testing.T) {
	env := newEnvState(0.01, 0.01, 48000, envPeak)
	env.step(1.0)
	if env.env == 0 {
		t.Fatal("envelope expected to be non-zero before reset")
	}

	env.reset()
	if env.env != 0 {
		t.Fatalf("envelope after reset = %v, want 0", env.env)
	}
}

// TestEnvelopeRateDefaultAndInstantaneous verifies rate fallback and zero-time coefficients.
func TestEnvelopeRateDefaultAndInstantaneous(t *testing.T) {
	env := newEnvState(0, -1, 0, envPeak)
	if env.attack != 1.0 {
		t.Fatalf("attack coeff = %v, want 1.0", env.attack)
	}
	if env.release != 1.0 {
		t.Fatalf("release coeff = %v, want 1.0", env.release)
	}

	if got := env.step(0.42); got != 0.42 {
		t.Fatalf("step = %v, want 0.42", got)
	}
	if got := env.step(0.15); got != 0.15 {
		t.Fatalf("step = %v, want 0.15", got)
	}
}
