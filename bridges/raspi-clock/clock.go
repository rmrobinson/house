package main

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/rafalop/sevensegment"
)

const (
	TwentyFourHour int = iota
	TwelveHour
)

var (
	colonOn12HrPM  = [7]bool{true, true, false, true, false, false, false}
	colonOff12HrPM = [7]bool{false, false, false, true, false, false, false}
	colonOn24Hr    = [7]bool{true, true, false, false, false, false, false}
	dateSeparator  = [7]bool{false, true, false, false, false, false, false}
)

// Clock displays time on a Raspberry Pi HT16K33 i2c-based Seven Segment Display.
type Clock struct {
	// a channel used to request the brightness be updated
	brightnessUpdates chan int
	// the current brightness, as a percentage between 0 and 100.
	currBrightness int

	// a channel used to control whether the display is on or off
	isOnUpdates chan bool
	// the current state of whether the clock is on or off
	isOn bool

	// whether we're going to display this in 12 or 24 hour mode
	timeMode int

	// what timezone to set this time in
	timezone *time.Location

	// the display being managed
	display *sevensegment.SevenSegment
}

// NewClock creates a new clock running in 24 hour time against the UTC location.
func NewClock(d *sevensegment.SevenSegment) *Clock {
	utc, err := time.LoadLocation("UTC")
	if err != nil {
		panic(err)
	}

	return &Clock{
		brightnessUpdates: make(chan int),
		currBrightness:    100,
		isOnUpdates:       make(chan bool),
		isOn:              true,
		timeMode:          TwentyFourHour,
		timezone:          utc,
		display:           d,
	}
}

// Run takes the supplied parameters and begins displaying the time.
// This will run until the supplied context is cancelled.
func (c *Clock) Run(ctx context.Context) {
	refreshTicker := time.NewTicker(time.Millisecond * 100)
	colonTicker := time.NewTicker(time.Millisecond * 500)

	colonOn := false

	c.display.Clear()
	c.display.SetBrightness(0)

	for {
		select {
		case <-ctx.Done():
			fmt.Printf("context cancelled\n")
		case brightness := <-c.brightnessUpdates:
			newBrightness := int(math.Round(float64(brightness) * 0.15))

			c.display.SetBrightness(15 - newBrightness)
			c.currBrightness = brightness
		case isOn := <-c.isOnUpdates:
			c.isOn = isOn

			if isOn {
				c.display.DisplayOn()
				c.isOn = true
			} else {
				c.display.Clear()
				c.isOn = false
			}
			// If isOn the next clock ticker will set it appropriately; we don't explicitly set 'on' here.
		case <-colonTicker.C:
			if !colonOn {
				colonOn = true
			} else {
				colonOn = false
			}
		case <-refreshTicker.C:
			if !c.isOn {
				continue
			}

			t := time.Now().In(c.timezone)
			hour := t.Hour()
			min := t.Minute()
			day := t.Day()
			month := t.Month()

			showDate := false
			if t.Second() > 50 {
				showDate = true
			}

			pmRequired := false
			if c.timeMode == TwelveHour && hour > 12 {
				hour = hour - 12
				pmRequired = true
			}

			separatorChar := ' '
			if colonOn {
				separatorChar = ':'
			}

			pmString := ""
			if pmRequired {
				pmString = "."
			}

			if showDate {
				c.display.SetDigit(0, int(month)%10)
				c.display.SetDigit(1, int(month)/10)
				c.display.SetDigit(2, day%10)
				c.display.SetDigit(3, day/10)

				c.display.SetSegments(4, dateSeparator)

				fmt.Printf("\033[2K\r%d%d%.d%d", int(month)/10, int(month)%10, day/10, day%10)
			} else {
				c.display.SetDigit(0, min%10)
				c.display.SetDigit(1, min/10)
				c.display.SetDigit(2, hour%10)
				c.display.SetDigit(3, hour/10)

				if colonOn && pmRequired {
					c.display.SetSegments(4, colonOn12HrPM)
				} else if colonOn {
					c.display.SetSegments(4, colonOn24Hr)
				} else if !colonOn && pmRequired {
					c.display.SetSegments(4, colonOff12HrPM)
				} else {
					c.display.SetSegments(4, [7]bool{false, false, false, false, false, false, false})
				}

				fmt.Printf("\033[2K\r%d%d%c%d%d%s", hour/10, hour%10, separatorChar, min/10, min%10, pmString)
			}

			c.display.WriteData()

		}
	}
}

// ChangeBrightness increases or decreases the brightness by the supplied percentage change from current.
func (c *Clock) ChangeBrightness(increment int) {
	c.brightnessUpdates <- c.currBrightness + increment
}

// SetBrightness sets the brightness to a percentage from 0 to 100. Default is 100.
func (c *Clock) SetBrightness(level int) {
	c.brightnessUpdates <- level
}

//SetTimeMode allows for the display time to be changed from 24 hour (default) to 12 hour.
func (c *Clock) SetTimeMode(mode int) {
	c.timeMode = mode
}

// SetTimeZone allows for the display time to be set for a given timezone locale string.
// If the requested timezone doesn't exist or is not formatted properly this will return an error.
func (c *Clock) SetTimeZone(tz string) error {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return err
	}

	c.timezone = loc
	return nil
}

// SetOnOff changes whether the clock display is on or off
func (c *Clock) SetOnOff(isOn bool) {
	c.isOnUpdates <- isOn
}
