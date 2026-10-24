// Package astro has the little astronomy skycam needs: sun altitude for the
// day/night switch, moon altitude and illumination for the cloud baseline,
// and allsky's night-date rule. Low-precision formulas (about 0.1° for the
// sun, under 1° for the moon) — plenty for a twilight threshold.
package astro

import (
	"math"
	"time"
)

const rad = math.Pi / 180

// daysJ2000 is days since 2000-01-01 12:00 UTC.
func daysJ2000(t time.Time) float64 {
	return float64(t.UTC().Unix()-946728000) / 86400
}

func wrap360(x float64) float64 {
	x = math.Mod(x, 360)
	if x < 0 {
		x += 360
	}
	return x
}

// altitude converts equatorial coordinates (degrees) to altitude for an
// observer at lat/lon at time t.
func altitude(t time.Time, lat, lon, ra, dec float64) float64 {
	d := daysJ2000(t)
	gmst := wrap360(280.46061837 + 360.98564736629*d)
	ha := (gmst + lon - ra) * rad
	s := math.Sin(lat*rad)*math.Sin(dec*rad) + math.Cos(lat*rad)*math.Cos(dec*rad)*math.Cos(ha)
	return math.Asin(s) / rad
}

func obliquity(d float64) float64 { return 23.439 - 0.00000036*d }

// sunEcliptic is the sun's ecliptic longitude in degrees.
func sunEcliptic(d float64) float64 {
	g := wrap360(357.529+0.98560028*d) * rad
	q := wrap360(280.459 + 0.98564736*d)
	return wrap360(q + 1.915*math.Sin(g) + 0.020*math.Sin(2*g))
}

func toEquatorial(lon, lat, eps float64) (ra, dec float64) {
	l, b, e := lon*rad, lat*rad, eps*rad
	ra = math.Atan2(math.Sin(l)*math.Cos(e)-math.Tan(b)*math.Sin(e), math.Cos(l)) / rad
	dec = math.Asin(math.Sin(b)*math.Cos(e)+math.Cos(b)*math.Sin(e)*math.Sin(l)) / rad
	return wrap360(ra), dec
}

// SunAltitude is the sun's altitude in degrees (no refraction).
func SunAltitude(t time.Time, lat, lon float64) float64 {
	d := daysJ2000(t)
	ra, dec := toEquatorial(sunEcliptic(d), 0, obliquity(d))
	return altitude(t, lat, lon, ra, dec)
}

// moonEcliptic is the moon's geocentric ecliptic longitude and latitude
// (Astronomical Almanac low-precision series).
func moonEcliptic(d float64) (lon, lat float64) {
	T := d / 36525
	s := func(a, b float64) float64 { return math.Sin(wrap360(a+b*T) * rad) }
	lon = 218.32 + 481267.881*T +
		6.29*s(134.9, 477198.85) - 1.27*s(259.2, -413335.38) +
		0.66*s(235.7, 890534.23) + 0.21*s(269.9, 954397.70) -
		0.19*s(357.5, 35999.05) - 0.11*s(186.6, 966404.05)
	lat = 5.13*s(93.3, 483202.03) + 0.28*s(228.2, 960400.87) -
		0.28*s(318.3, 6003.18) - 0.17*s(217.6, -407332.20)
	return wrap360(lon), lat
}

// MoonAltitude is the moon's altitude in degrees (geocentric, so up to
// about 1° high near the horizon).
func MoonAltitude(t time.Time, lat, lon float64) float64 {
	d := daysJ2000(t)
	ml, mb := moonEcliptic(d)
	ra, dec := toEquatorial(ml, mb, obliquity(d))
	return altitude(t, lat, lon, ra, dec)
}

// MoonIllumination is the lit fraction of the moon's disc, 0 (new) to 1
// (full).
func MoonIllumination(t time.Time) float64 {
	d := daysJ2000(t)
	ml, mb := moonEcliptic(d)
	elong := math.Acos(math.Cos(mb*rad) * math.Cos((ml-sunEcliptic(d))*rad))
	return (1 - math.Cos(elong)) / 2
}

// NightDate is allsky's folder date: the date the night began. Anything
// before noon belongs to the previous evening, so a 02:00 frame on the
// 24th files under the 23rd.
func NightDate(t time.Time, loc *time.Location) string {
	return t.In(loc).Add(-12 * time.Hour).Format("20060102")
}
