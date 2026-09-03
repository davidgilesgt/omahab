package ui

import (
	"image/color"
	"math"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/lucasb-eyer/go-colorful"
)

// Palette holds all colors used by the UI. ANSI color fields always contain
// BasicColor values so the terminal applies its own theme. Synthesized
// colors (FocusOrange, BackgroundTint, LightText) are true-color RGB.
type Palette struct {
	// ANSI 16 — always BasicColor values for rendering
	Black, Red, Green, Yellow, Blue, Magenta, Cyan, White                                                 color.Color
	BrightBlack, BrightRed, BrightGreen, BrightYellow, BrightBlue, BrightMagenta, BrightCyan, BrightWhite color.Color

	// Synthesized (always true-color RGB)
	FocusOrange    color.Color
	BackgroundTint color.Color
	LightText      color.Color

	// Semantic aliases
	Border  color.Color // = LightText
	Muted   color.Color // = LightText
	Focused color.Color // = FocusOrange
	Primary color.Color // = Blue or BrightBlue (better contrast)
	Error   color.Color // = Red
	Success color.Color // = Green
	Warning color.Color // = FocusOrange
	// Private: detected RGB samples for calculations
	samples  [sampleCount]colorful.Color
	detected [sampleCount]bool
	isDark   bool
}

// Gradient interpolates between green and FocusOrange in OKLCH.
// t=0 returns green, t=1 returns orange.
func (p *Palette) Gradient(t float64) color.Color {
	t = max(0, min(1, t))
	greenSample := p.samples[int(ansi.Green)]
	orangeSample, _ := colorful.MakeColor(p.FocusOrange)
	return greenSample.BlendOkLch(orangeSample, t)
}

// HealthColor returns the palette color for the given health state.
func (p *Palette) HealthColor(h HealthState) color.Color {
	switch h {
	case healthWarning:
		return p.Warning
	case healthError:
		return p.Error
	default:
		return p.Success
	}
}

// DefaultPalette returns a palette with ANSI BasicColor values and
// fallback-derived synthesized colors. This is the package-init value.
func DefaultPalette() *Palette {
	p := &Palette{
		Black:         lipgloss.Black,
		Red:           lipgloss.Red,
		Green:         lipgloss.Green,
		Yellow:        lipgloss.Yellow,
		Blue:          lipgloss.Blue,
		Magenta:       lipgloss.Magenta,
		Cyan:          lipgloss.Cyan,
		White:         lipgloss.White,
		BrightBlack:   lipgloss.BrightBlack,
		BrightRed:     lipgloss.BrightRed,
		BrightGreen:   lipgloss.BrightGreen,
		BrightYellow:  lipgloss.BrightYellow,
		BrightBlue:    lipgloss.BrightBlue,
		BrightMagenta: lipgloss.BrightMagenta,
		BrightCyan:    lipgloss.BrightCyan,
		BrightWhite:   lipgloss.BrightWhite,
		isDark:        true,
	}

	p.samples = defaultSamples()
	p.synthesize()
	return p
}

// NewPalette creates a palette from detected terminal colors.
func NewPalette(detected DetectedColors) *Palette {
	p := DefaultPalette()

	p.detected = detected.Detected
	defaults := defaultSamples()

	for i := range sampleCount {
		if detected.Detected[i] {
			p.samples[i] = detected.Colors[i]
		} else {
			p.samples[i] = defaults[i]
		}
	}

	if detected.Detected[sampleBackground] {
		l, _, _ := detected.Colors[sampleBackground].OkLch()
		p.isDark = l < 0.5
	}

	p.Primary = pickPrimary(p)
	p.synthesize()

	if !detected.SupportsTrueColor() {
		p.BackgroundTint = nil
	}

	return p
}

// ApplyPalette sets the package-level Colors variable and rebuilds
// all package-level style variables that depend on colors.
func ApplyPalette(p *Palette) {
	Colors = p
	rebuildStyles()
}

// Private

func (p *Palette) synthesize() {
	p.FocusOrange = synthesizeOrange(p.samples[int(ansi.Blue)])
	p.BackgroundTint = synthesizeTint(p.samples[sampleBackground])
	p.LightText = synthesizeLightText(
		p.samples[sampleBackground],
		p.samples[sampleForeground],
		p.samples[int(ansi.Blue)],
	)

	p.Border = p.LightText
	p.Muted = p.LightText
	p.Focused = p.FocusOrange
	if p.Primary == nil {
		p.Primary = lipgloss.BrightBlue
	}
	p.Error = p.Red
	p.Success = p.Green
	p.Warning = p.FocusOrange
}

// synthesizeOrange produces a warm orange as the OKLCH complement of blue,
// with hue clamped to the 35°–75° range.
func synthesizeOrange(blue colorful.Color) color.Color {
	l, c, h := blue.OkLch()

	// Complement: rotate 180°
	h = math.Mod(h+180, 360)

	// Clamp hue to orange band
	h = max(35, min(75, h))

	// Ensure usable chroma and lightness
	c = max(c, 0.10)
	l = max(0.55, min(0.85, l))

	return colorful.OkLch(l, c, h).Clamped()
}

// synthesizeTint darkens the background by an absolute OKLCH lightness delta.
func synthesizeTint(bg colorful.Color) color.Color {
	l, c, h := bg.OkLch()
	l = max(l-0.015, 0)
	return colorful.OkLch(l, c, h).Clamped()
}

// synthesizeLightText produces a subdued blue-grey for secondary text.
// It blends 35% from background toward foreground in lightness,
// with a touch of chroma on the blue axis.
func synthesizeLightText(bg, fg, blue colorful.Color) color.Color {
	bgL, _, _ := bg.OkLch()
	fgL, _, _ := fg.OkLch()
	_, blueC, blueH := blue.OkLch()

	l := bgL + 0.35*(fgL-bgL)
	c := min(blueC*0.15, 0.04)
	return colorful.OkLch(l, c, blueH).Clamped()
}

// pickPrimary chooses the better of Blue and BrightBlue for contrast
// against the background. Falls back to BrightBlue when detection is
// incomplete.
func pickPrimary(p *Palette) color.Color {
	bothDetected := p.detected[int(ansi.Blue)] &&
		p.detected[int(ansi.BrightBlue)] &&
		p.detected[sampleBackground]

	if !bothDetected {
		return lipgloss.BrightBlue
	}

	bg := p.samples[sampleBackground]
	bgL, _, _ := bg.OkLch()

	blueL, _, _ := p.samples[int(ansi.Blue)].OkLch()
	brightL, _, _ := p.samples[int(ansi.BrightBlue)].OkLch()

	if math.Abs(brightL-bgL) >= math.Abs(blueL-bgL) {
		return lipgloss.BrightBlue
	}
	return lipgloss.Blue
}

// defaultSamples returns fallback RGB values for OKLCH calculations.
// These are never emitted to the terminal.
func defaultSamples() [sampleCount]colorful.Color {
	hex := func(s string) colorful.Color {
		c, _ := colorful.Hex(s)
		return c
	}

	var s [sampleCount]colorful.Color
	// Standard dark-theme defaults (xterm-like)
	s[int(ansi.Black)] = hex("#000000")
	s[int(ansi.Red)] = hex("#cc0000")
	s[int(ansi.Green)] = hex("#50fa7b")
	s[int(ansi.Yellow)] = hex("#cdcd00")
	s[int(ansi.Blue)] = hex("#7AA2F7")
	s[int(ansi.Magenta)] = hex("#cd00cd")
	s[int(ansi.Cyan)] = hex("#00cdcd")
	s[int(ansi.White)] = hex("#e5e5e5")
	s[int(ansi.BrightBlack)] = hex("#7f7f7f")
	s[int(ansi.BrightRed)] = hex("#ff0000")
	s[int(ansi.BrightGreen)] = hex("#00ff00")
	s[int(ansi.BrightYellow)] = hex("#ffff00")
	s[int(ansi.BrightBlue)] = hex("#5c5cff")
	s[int(ansi.BrightMagenta)] = hex("#ff00ff")
	s[int(ansi.BrightCyan)] = hex("#00ffff")
	s[int(ansi.BrightWhite)] = hex("#ffffff")
	s[sampleForeground] = hex("#c0caf5")
	s[sampleBackground] = hex("#1a1b26")
	return s
}
