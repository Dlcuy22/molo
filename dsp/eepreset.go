// Clean-room note: this file reads the EasyEffects preset JSON format specification.
// It is an independent clean-room implementation and does not copy EasyEffects code.

package dsp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// ErrInvalidPreset indicates the JSON document is not a valid EasyEffects preset.
var ErrInvalidPreset = errors.New("dsp: invalid EasyEffects preset")

// LoadEasyEffectsPreset parses an EasyEffects preset JSON document into a Pipeline.
// It maps the output chain into post-processing stages and collects unmapped settings as warnings.
func LoadEasyEffectsPreset(data []byte) (Pipeline, []string, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return Pipeline{}, nil, fmt.Errorf("%w: %v", ErrInvalidPreset, err)
	}

	outputRaw, ok := root["output"]
	if !ok || len(outputRaw) == 0 || string(outputRaw) == "null" {
		return Pipeline{}, nil, fmt.Errorf("%w: missing output section", ErrInvalidPreset)
	}

	var outputMap map[string]json.RawMessage
	if err := json.Unmarshal(outputRaw, &outputMap); err != nil {
		return Pipeline{}, nil, fmt.Errorf("%w: output is not a JSON object: %v", ErrInvalidPreset, err)
	}

	pluginsOrderRaw, ok := outputMap["plugins_order"]
	if !ok || len(pluginsOrderRaw) == 0 || string(pluginsOrderRaw) == "null" {
		return Pipeline{}, nil, fmt.Errorf("%w: missing plugins_order in output section", ErrInvalidPreset)
	}

	var pluginsOrder []string
	if err := json.Unmarshal(pluginsOrderRaw, &pluginsOrder); err != nil {
		return Pipeline{}, nil, fmt.Errorf("%w: plugins_order is not an array of strings: %v", ErrInvalidPreset, err)
	}

	var warnings []string
	var postSpecs []Spec
	seenStages := make(map[string]struct{}, len(pluginsOrder))

	for _, stageID := range pluginsOrder {
		if _, seen := seenStages[stageID]; seen {
			warnings = append(warnings, fmt.Sprintf("%s skipped: duplicate stage in plugins_order", stageID))
			continue
		}
		seenStages[stageID] = struct{}{}

		stageRaw, exists := outputMap[stageID]
		if !exists {
			warnings = append(warnings, fmt.Sprintf("%s skipped: missing stage configuration in preset", stageID))
			continue
		}

		var stageMap map[string]any
		if err := json.Unmarshal(stageRaw, &stageMap); err != nil || stageMap == nil {
			warnings = append(warnings, fmt.Sprintf("%s skipped: invalid stage JSON", stageID))
			continue
		}

		baseKind := stageID
		if idx := strings.Index(stageID, "#"); idx != -1 {
			baseKind = stageID[:idx]
		}

		spec, stageWarnings, ok := mapStage(stageID, baseKind, stageMap)
		warnings = append(warnings, stageWarnings...)
		if ok {
			postSpecs = append(postSpecs, spec)
		}
	}

	return Pipeline{Post: postSpecs}, warnings, nil
}

// LoadEasyEffectsPresetFile reads an EasyEffects preset from a file on disk.
func LoadEasyEffectsPresetFile(path string) (Pipeline, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Pipeline{}, nil, fmt.Errorf("dsp: read preset file: %w", err)
	}

	return LoadEasyEffectsPreset(data)
}

func mapStage(stageID, baseKind string, raw map[string]any) (Spec, []string, bool) {
	switch baseKind {
	case "crossfeed":
		return mapCrossfeed(stageID, raw)
	case "equalizer":
		return mapEqualizer(stageID, raw)
	case "convolver":
		return mapConvolver(stageID, raw)
	case "bass_enhancer", "bass-enhancer":
		return mapBassEnhancer(stageID, raw)
	case "maximizer":
		return mapMaximizer(stageID, raw)
	case "limiter":
		return mapLimiter(stageID, raw)
	default:
		return Spec{}, []string{fmt.Sprintf("%s skipped: unknown effect kind", stageID)}, false
	}
}

func mapCommonParam(stageID, k string, val any, params Values, warnings *[]string) bool {
	switch k {
	case ParamBypass:
		if b, ok := val.(bool); ok {
			params[ParamBypass] = b
		} else {
			*warnings = append(*warnings, fmt.Sprintf("%s.bypass skipped: invalid bool value", stageID))
		}
		return true
	case ParamInputGain:
		if f, ok := asFloat(val); ok {
			params[ParamInputGain] = f
		} else {
			*warnings = append(*warnings, fmt.Sprintf("%s.input-gain skipped: invalid float value", stageID))
		}
		return true
	case ParamOutputGain:
		if f, ok := asFloat(val); ok {
			params[ParamOutputGain] = f
		} else {
			*warnings = append(*warnings, fmt.Sprintf("%s.output-gain skipped: invalid float value", stageID))
		}
		return true
	default:
		return false
	}
}

func mapCrossfeed(stageID string, raw map[string]any) (Spec, []string, bool) {
	params := make(Values)
	var warnings []string

	for _, k := range sortedKeys(raw) {
		val := raw[k]
		if mapCommonParam(stageID, k, val, params, &warnings) {
			continue
		}
		switch k {
		case "fcut", "cutoff":
			if f, ok := asFloat(val); ok {
				params[CrossfeedCutoff] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.%s skipped: invalid float value", stageID, k))
			}
		case "feed":
			if f, ok := asFloat(val); ok {
				params[CrossfeedFeed] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.feed skipped: invalid float value", stageID))
			}
		default:
			warnings = append(warnings, fmt.Sprintf("%s.%s skipped: unsupported parameter", stageID, k))
		}
	}

	return Spec{ID: stageID, Kind: "crossfeed", Params: params}, warnings, true
}

func mapEqualizer(stageID string, raw map[string]any) (Spec, []string, bool) {
	params := make(Values)
	var warnings []string

	var leftRaw any
	var rightRaw any
	var eeNumBands int

	for _, k := range sortedKeys(raw) {
		val := raw[k]
		if mapCommonParam(stageID, k, val, params, &warnings) {
			continue
		}
		switch k {
		case "left":
			leftRaw = val
		case "right":
			rightRaw = val
		case "num-bands":
			if f, ok := asFloat(val); ok {
				eeNumBands = int(f)
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.num-bands skipped: invalid int value", stageID))
			}
		case "mode":
			warnings = append(warnings, fmt.Sprintf("%s.mode skipped: unsupported parameter", stageID))
		case "split-channels":
			warnings = append(warnings, fmt.Sprintf("%s.split-channels skipped: unsupported parameter", stageID))
		default:
			warnings = append(warnings, fmt.Sprintf("%s.%s skipped: unsupported parameter", stageID, k))
		}
	}

	var bandsMap map[string]any
	if leftRaw != nil {
		if m, ok := leftRaw.(map[string]any); ok {
			bandsMap = m
			if rightRaw != nil {
				warnings = append(warnings, fmt.Sprintf("%s.right skipped: right channel bands ignored (split channels not supported)", stageID))
			}
		} else {
			warnings = append(warnings, fmt.Sprintf("%s.left skipped: invalid bands object", stageID))
		}
	}

	if bandsMap == nil {
		if rightRaw != nil {
			if m, ok := rightRaw.(map[string]any); ok {
				bandsMap = m
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.right skipped: invalid bands object", stageID))
			}
		} else if leftRaw == nil {
			warnings = append(warnings, fmt.Sprintf("%s.left skipped: missing bands configuration", stageID))
		}
	}

	maxBandIdx := -1
	if bandsMap != nil {
		for _, k := range sortedKeys(bandsMap) {
			if !strings.HasPrefix(k, "band") {
				warnings = append(warnings, fmt.Sprintf("%s.%s skipped: unsupported parameter", stageID, k))
				continue
			}
			idx, err := strconv.Atoi(k[4:])
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s.%s skipped: unsupported parameter", stageID, k))
				continue
			}
			if idx < 0 || idx >= equalizerMaxBands {
				warnings = append(warnings, fmt.Sprintf("%s.%s skipped: band index out of range (0..%d)", stageID, k, equalizerMaxBands-1))
				continue
			}

			bandObj, ok := bandsMap[k].(map[string]any)
			if !ok {
				warnings = append(warnings, fmt.Sprintf("%s.%s skipped: invalid band object", stageID, k))
				continue
			}

			if idx > maxBandIdx {
				maxBandIdx = idx
			}

			for _, subk := range sortedKeys(bandObj) {
				subval := bandObj[subk]
				switch subk {
				case "frequency":
					if f, ok := asFloat(subval); ok {
						params[EqualizerBandFreqKey(idx)] = f
					} else {
						warnings = append(warnings, fmt.Sprintf("%s.%s.frequency skipped: invalid float value", stageID, k))
					}
				case "gain":
					if f, ok := asFloat(subval); ok {
						params[EqualizerBandGainKey(idx)] = f
					} else {
						warnings = append(warnings, fmt.Sprintf("%s.%s.gain skipped: invalid float value", stageID, k))
					}
				case "q":
					if f, ok := asFloat(subval); ok {
						params[EqualizerBandQKey(idx)] = f
					} else {
						warnings = append(warnings, fmt.Sprintf("%s.%s.q skipped: invalid float value", stageID, k))
					}
				case "mode":
					warnings = append(warnings, fmt.Sprintf("%s.%s.mode skipped: %v curve not representable", stageID, k, subval))
				default:
					warnings = append(warnings, fmt.Sprintf("%s.%s.%s skipped: unsupported parameter", stageID, k, subk))
				}
			}
		}
	}

	numBands := equalizerDefaultBands
	if eeNumBands > 0 {
		numBands = min(eeNumBands, equalizerMaxBands)
	} else if maxBandIdx >= 0 {
		numBands = min(maxBandIdx+1, equalizerMaxBands)
	}
	if numBands < equalizerMinBands {
		numBands = equalizerMinBands
	}
	params[EqualizerNumBands] = numBands

	return Spec{ID: stageID, Kind: "equalizer", Params: params}, warnings, true
}

func mapConvolver(stageID string, raw map[string]any) (Spec, []string, bool) {
	params := make(Values)
	var warnings []string

	for _, k := range sortedKeys(raw) {
		val := raw[k]
		if mapCommonParam(stageID, k, val, params, &warnings) {
			continue
		}
		switch k {
		case "kernel-name", "kernel":
			name, ok := val.(string)
			if !ok {
				warnings = append(warnings, fmt.Sprintf("%s.%s skipped: invalid string value", stageID, k))
			} else if slices.Contains(kernels(), name) {
				params[ConvolverKernel] = name
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.%s skipped: kernel %q not registered", stageID, k, name))
			}
		case "ir-width":
			if f, ok := asFloat(val); ok {
				params[ConvolverIRWidth] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.ir-width skipped: invalid float value", stageID))
			}
		case "autogain":
			if b, ok := val.(bool); ok {
				params[ConvolverAutogain] = b
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.autogain skipped: invalid bool value", stageID))
			}
		default:
			warnings = append(warnings, fmt.Sprintf("%s.%s skipped: unsupported parameter", stageID, k))
		}
	}

	return Spec{ID: stageID, Kind: "convolver", Params: params}, warnings, true
}

func mapBassEnhancer(stageID string, raw map[string]any) (Spec, []string, bool) {
	params := make(Values)
	var warnings []string

	for _, k := range sortedKeys(raw) {
		val := raw[k]
		if mapCommonParam(stageID, k, val, params, &warnings) {
			continue
		}
		switch k {
		case BassEnhancerAmount:
			if f, ok := asFloat(val); ok {
				params[BassEnhancerAmount] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.amount skipped: invalid float value", stageID))
			}
		case BassEnhancerHarmonics:
			if f, ok := asFloat(val); ok {
				params[BassEnhancerHarmonics] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.harmonics skipped: invalid float value", stageID))
			}
		case BassEnhancerScope:
			if f, ok := asFloat(val); ok {
				params[BassEnhancerScope] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.scope skipped: invalid float value", stageID))
			}
		case BassEnhancerFloor:
			if f, ok := asFloat(val); ok {
				params[BassEnhancerFloor] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.floor skipped: invalid float value", stageID))
			}
		case BassEnhancerFloorActive:
			if b, ok := val.(bool); ok {
				params[BassEnhancerFloorActive] = b
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.floor-active skipped: invalid bool value", stageID))
			}
		case BassEnhancerBlend:
			if f, ok := asFloat(val); ok {
				params[BassEnhancerBlend] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.blend skipped: invalid float value", stageID))
			}
		case BassEnhancerListen:
			if b, ok := val.(bool); ok {
				params[BassEnhancerListen] = b
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.listen skipped: invalid bool value", stageID))
			}
		default:
			warnings = append(warnings, fmt.Sprintf("%s.%s skipped: unsupported parameter", stageID, k))
		}
	}

	return Spec{ID: stageID, Kind: "bass-enhancer", Params: params}, warnings, true
}

func mapMaximizer(stageID string, raw map[string]any) (Spec, []string, bool) {
	params := make(Values)
	var warnings []string

	for _, k := range sortedKeys(raw) {
		val := raw[k]
		if mapCommonParam(stageID, k, val, params, &warnings) {
			continue
		}
		switch k {
		case MaximizerThreshold:
			if f, ok := asFloat(val); ok {
				params[MaximizerThreshold] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.threshold skipped: invalid float value", stageID))
			}
		case MaximizerRelease:
			if f, ok := asFloat(val); ok {
				params[MaximizerRelease] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.release skipped: invalid float value", stageID))
			}
		case MaximizerCeiling:
			if f, ok := asFloat(val); ok {
				params[MaximizerCeiling] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.ceiling skipped: invalid float value", stageID))
			}
		default:
			warnings = append(warnings, fmt.Sprintf("%s.%s skipped: unsupported parameter", stageID, k))
		}
	}

	return Spec{ID: stageID, Kind: "maximizer", Params: params}, warnings, true
}

func mapLimiter(stageID string, raw map[string]any) (Spec, []string, bool) {
	params := make(Values)
	var warnings []string

	for _, k := range sortedKeys(raw) {
		val := raw[k]
		if mapCommonParam(stageID, k, val, params, &warnings) {
			continue
		}
		if strings.HasPrefix(k, "alr") {
			warnings = append(warnings, fmt.Sprintf("%s.%s skipped: ALR not supported", stageID, k))
			continue
		}
		switch k {
		case LimiterThreshold:
			if f, ok := asFloat(val); ok {
				params[LimiterThreshold] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.threshold skipped: invalid float value", stageID))
			}
		case LimiterAttack:
			if f, ok := asFloat(val); ok {
				params[LimiterAttack] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.attack skipped: invalid float value", stageID))
			}
		case LimiterRelease:
			if f, ok := asFloat(val); ok {
				params[LimiterRelease] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.release skipped: invalid float value", stageID))
			}
		case LimiterLookahead:
			if f, ok := asFloat(val); ok {
				params[LimiterLookahead] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.lookahead skipped: invalid float value", stageID))
			}
		case LimiterStereoLink:
			if f, ok := asFloat(val); ok {
				params[LimiterStereoLink] = f
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.stereo-link skipped: invalid float value", stageID))
			}
		case LimiterMode:
			s, ok := val.(string)
			if !ok {
				warnings = append(warnings, fmt.Sprintf("%s.mode skipped: invalid string value", stageID))
			} else if slices.Contains(limiterModeOptions, s) {
				params[LimiterMode] = s
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.mode skipped: unsupported mode option %q", stageID, s))
			}
		case LimiterOversampling:
			s, ok := val.(string)
			if !ok {
				warnings = append(warnings, fmt.Sprintf("%s.oversampling skipped: invalid string value", stageID))
			} else if slices.Contains(limiterOversamplingOptions, s) {
				params[LimiterOversampling] = s
			} else {
				warnings = append(warnings, fmt.Sprintf("%s.oversampling skipped: unsupported oversampling option %q", stageID, s))
			}
		default:
			warnings = append(warnings, fmt.Sprintf("%s.%s skipped: unsupported parameter", stageID, k))
		}
	}

	return Spec{ID: stageID, Kind: "limiter", Params: params}, warnings, true
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
