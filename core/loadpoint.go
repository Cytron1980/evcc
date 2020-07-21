package core

import (
	"time"

	"github.com/andig/evcc/api"
	"github.com/andig/evcc/core/wrapper"
	"github.com/andig/evcc/push"
	"github.com/andig/evcc/util"
	"github.com/pkg/errors"

	evbus "github.com/asaskevich/EventBus"
	"github.com/avast/retry-go"
	"github.com/benbjohnson/clock"
)

const (
	evChargeStart       = "start"      // update chargeTimer
	evChargeStop        = "stop"       // update chargeTimer
	evChargeCurrent     = "current"    // update fakeChargeMeter
	evChargePower       = "power"      // update chargeRater
	evVehicleConnect    = "connect"    // vehicle connected
	evVehicleDisconnect = "disconnect" // vehicle disconnected

	minActiveCurrent = 1 // minimum current at which a phase is treated as active
)

// ThresholdConfig defines enable/disable hysteresis parameters
type ThresholdConfig struct {
	Delay     time.Duration
	Threshold float64
}

// LoadPoint is responsible for controlling charge depending on
// SoC needs and power availability.
type LoadPoint struct {
	clock    clock.Clock       // mockable time
	bus      evbus.Bus         // event bus
	pushChan chan<- push.Event // notifications
	uiChan   chan<- util.Param // client push messages
	log      *util.Logger

	// exposed public configuration
	Title      string `mapstructure:"title"`   // UI title
	Phases     int64  `mapstructure:"phases"`  // Phases- required for converting power and current
	ChargerRef string `mapstructure:"charger"` // Charger reference
	VehicleRef string `mapstructure:"vehicle"` // Vehicle reference
	Meters     struct {
		ChargeMeterRef string `mapstructure:"charge"` // Charge meter reference
	}
	Enable, Disable ThresholdConfig

	handler       Handler
	HandlerConfig `mapstructure:",squash"` // handle charger state and current

	chargeTimer api.ChargeTimer
	chargeRater api.ChargeRater

	chargeMeter api.Meter   // Charger usage meter
	vehicle     api.Vehicle // Vehicle

	// cached state
	status        api.ChargeStatus // Charger status
	charging      bool             // Charging cycle
	chargePower   float64          // Charging power
	connectedTime time.Time        // time vehicle was connected

	pvTimer time.Time
}

// NewLoadPointFromConfig creates a new loadpoint
func NewLoadPointFromConfig(log *util.Logger, cp configProvider, other map[string]interface{}) *LoadPoint {
	lp := NewLoadPoint(log)
	util.DecodeOther(log, other, &lp)

	if lp.Meters.ChargeMeterRef != "" {
		lp.chargeMeter = cp.Meter(lp.Meters.ChargeMeterRef)
	}
	if lp.VehicleRef != "" {
		lp.vehicle = cp.Vehicle(lp.VehicleRef)
	}

	if lp.ChargerRef == "" {
		lp.log.FATAL.Fatal("config: missing charger")
	}
	charger := cp.Charger(lp.ChargerRef)
	lp.configureChargerType(charger)

	if lp.Enable.Threshold > lp.Disable.Threshold {
		log.WARN.Printf("PV mode enable threshold (%.0fW) is larger than disable threshold (%.0fW)", lp.Enable.Threshold, lp.Disable.Threshold)
	}

	lp.handler = &ChargerHandler{
		log:           lp.log,
		clock:         lp.clock,
		bus:           lp.bus,
		charger:       charger,
		HandlerConfig: lp.HandlerConfig,
	}

	return lp
}

// NewLoadPoint creates a LoadPoint with sane defaults
func NewLoadPoint(log *util.Logger) *LoadPoint {
	clock := clock.New()
	bus := evbus.New()

	lp := &LoadPoint{
		log:    log,   // logger
		clock:  clock, // mockable time
		bus:    bus,   // event bus
		Phases: 1,
		status: api.StatusNone,
		HandlerConfig: HandlerConfig{
			MinCurrent:    6,  // A
			MaxCurrent:    16, // A
			Sensitivity:   10, // A
			GuardDuration: 5 * time.Minute,
		},
	}

	return lp
}

// configureChargerType ensures that chargeMeter, Rate and Timer can use charger capabilities
func (lp *LoadPoint) configureChargerType(charger api.Charger) {
	// ensure charge meter exists
	if lp.chargeMeter == nil {
		if mt, ok := charger.(api.Meter); ok {
			lp.chargeMeter = mt
		} else {
			mt := &wrapper.ChargeMeter{}
			_ = lp.bus.Subscribe(evChargeCurrent, lp.evChargeCurrentHandler)
			_ = lp.bus.Subscribe(evChargeStop, func() {
				mt.SetPower(0)
			})
			lp.chargeMeter = mt
		}
	}

	// ensure charge rater exists
	if rt, ok := charger.(api.ChargeRater); ok {
		lp.chargeRater = rt
	} else {
		rt := wrapper.NewChargeRater(lp.log, lp.chargeMeter)
		_ = lp.bus.Subscribe(evChargePower, rt.SetChargePower)
		_ = lp.bus.Subscribe(evChargeStart, rt.StartCharge)
		_ = lp.bus.Subscribe(evChargeStop, rt.StopCharge)
		lp.chargeRater = rt
	}

	// ensure charge timer exists
	if ct, ok := charger.(api.ChargeTimer); ok {
		lp.chargeTimer = ct
	} else {
		ct := wrapper.NewChargeTimer()
		_ = lp.bus.Subscribe(evChargeStart, ct.StartCharge)
		_ = lp.bus.Subscribe(evChargeStop, ct.StopCharge)
		lp.chargeTimer = ct
	}
}

// notify sends push messages to clients
func (lp *LoadPoint) notify(event string) {
	lp.pushChan <- push.Event{Event: event}
}

// publish sends values to UI and databases
func (lp *LoadPoint) publish(key string, val interface{}) {
	lp.uiChan <- util.Param{Key: key, Val: val}
}

// evChargeStartHandler sends external start event
func (lp *LoadPoint) evChargeStartHandler() {
	lp.log.INFO.Println("start charging ->")
	lp.notify(evChargeStart)
}

// evChargeStopHandler sends external stop event
func (lp *LoadPoint) evChargeStopHandler() {
	lp.log.INFO.Println("stop charging <-")
	lp.publishChargeProgress()
	lp.notify(evChargeStop)
}

// evVehicleConnectHandler sends external start event
func (lp *LoadPoint) evVehicleConnectHandler() {
	lp.log.INFO.Printf("car connected")
	connectedDuration := lp.clock.Since(lp.connectedTime)
	lp.publish("connectedDuration", connectedDuration)
	lp.notify(evVehicleConnect)
}

// evVehicleDisconnectHandler sends external start event
func (lp *LoadPoint) evVehicleDisconnectHandler() {
	lp.log.INFO.Println("car disconnected")
	lp.connectedTime = lp.clock.Now()
	lp.notify(evVehicleDisconnect)
}

// evChargeCurrentHandler updates the dummy charge meter's charge power. This simplifies the main flow
// where the charge meter can always be treated as present. It assumes that the charge meter cannot consume
// more than total household consumption. If physical charge meter is present this handler is not used.
func (lp *LoadPoint) evChargeCurrentHandler(current int64) {
	power := float64(current*lp.Phases) * Voltage

	if !lp.handler.Enabled() || lp.status != api.StatusC {
		// if disabled we cannot be charging
		power = 0
	}
	// TODO
	// else if power > 0 && lp.Site.pvMeter != nil {
	// 	// limit charge power to generation plus grid consumption/ minus grid delivery
	// 	// as the charger cannot have consumed more than that
	// 	// consumedPower := consumedPower(lp.pvPower, lp.batteryPower, lp.gridPower)
	// 	consumedPower := lp.Site.consumedPower()
	// 	power = math.Min(power, consumedPower)
	// }

	// handler only called if charge meter was replaced by dummy
	lp.chargeMeter.(*wrapper.ChargeMeter).SetPower(power)

	// expose for UI
	lp.publish("chargeCurrent", current)
}

// Name returns the human-readable loadpoint title
func (lp *LoadPoint) Name() string {
	return lp.Title
}

// Prepare loadpoint configuration by adding missing helper elements
func (lp *LoadPoint) Prepare(uiChan chan<- util.Param, pushChan chan<- push.Event) {
	lp.pushChan = pushChan
	lp.uiChan = uiChan

	// event handlers
	_ = lp.bus.Subscribe(evChargeStart, lp.evChargeStartHandler)
	_ = lp.bus.Subscribe(evChargeStop, lp.evChargeStopHandler)

	// prepare charger status
	lp.handler.Prepare()
}

// connected returns the EVs connection state
func (lp *LoadPoint) connected() bool {
	return lp.status == api.StatusB || lp.status == api.StatusC
}

// updateChargeStatus updates car status and detects car connected/disconnected events
func (lp *LoadPoint) updateChargeStatus() error {
	status, err := lp.handler.Status()
	if err != nil {
		return err
	}

	lp.log.DEBUG.Printf("charger status: %s", status)

	if prevStatus := lp.status; status != prevStatus {
		lp.status = status

		// changed from A - connected
		if prevStatus == api.StatusA {
			lp.bus.Publish(evVehicleConnect)
		}

		// changed to A -  disconnected
		if status == api.StatusA {
			lp.bus.Publish(evVehicleDisconnect)
		}

		// update whenever there is a state change
		lp.bus.Publish(evChargeCurrent, lp.handler.TargetCurrent())

		// start/stop charging cycle
		if lp.charging = status == api.StatusC; lp.charging {
			lp.bus.Publish(evChargeStart)
		} else {
			// omit initial stop event before started
			if prevStatus != api.StatusNone {
				lp.bus.Publish(evChargeStop)
			}
		}
	}

	return nil
}

// detectPhases uses MeterCurrent interface to count phases with current >=1A
func (lp *LoadPoint) detectPhases() {
	if phaseMeter, ok := lp.chargeMeter.(api.MeterCurrent); ok {
		i1, i2, i3, err := phaseMeter.Currents()
		if err != nil {
			lp.log.ERROR.Printf("charge meter error: %v", err)
			return
		}

		var phases int64
		for _, i := range []float64{i1, i2, i3} {
			if i >= minActiveCurrent {
				phases++
			}
		}

		if phases > 0 {
			lp.Phases = min(phases, lp.Phases)
			lp.log.TRACE.Printf("detected phases: %d (%v)", lp.Phases, []float64{i1, i2, i3})

			lp.publish("activePhases", lp.Phases)
		}
	}
}

// maxCurrent calculates the maximum target current for PV mode
func (lp *LoadPoint) maxCurrent(mode api.ChargeMode, sitePower float64) int64 {
	// calculate target charge current from delta power and actual current
	effectiveCurrent := lp.handler.TargetCurrent()
	if lp.status != api.StatusC {
		effectiveCurrent = 0
	}
	deltaCurrent := powerToCurrent(-sitePower, lp.Phases)
	targetCurrent := clamp(effectiveCurrent+deltaCurrent, 0, lp.MaxCurrent)

	lp.log.DEBUG.Printf("max charge current: %dA = %dA + %dA (%.0fW @ %dp)", targetCurrent, effectiveCurrent, deltaCurrent, sitePower, lp.Phases)

	// in MinPV mode return at least minCurrent
	if mode == api.ModeMinPV && targetCurrent < lp.MinCurrent {
		return lp.MinCurrent
	}

	// in PV mode disable if not connected and minCurrent not possible
	if mode == api.ModePV && lp.status != api.StatusC {
		lp.pvTimer = time.Time{}

		if targetCurrent < lp.MinCurrent {
			return 0
		}

		return lp.MinCurrent
	}

	// read only once to simplify testing
	enabled := lp.handler.Enabled()

	if mode == api.ModePV && enabled && targetCurrent < lp.MinCurrent {
		// kick off disable sequence
		if sitePower >= lp.Disable.Threshold {
			lp.log.DEBUG.Printf("site power %.0fW >= disable threshold %.0fW", sitePower, lp.Disable.Threshold)

			if lp.pvTimer.IsZero() {
				lp.log.DEBUG.Println("start pv disable timer")
				lp.pvTimer = lp.clock.Now()
			}

			if lp.clock.Since(lp.pvTimer) >= lp.Disable.Delay {
				lp.log.DEBUG.Println("pv disable timer elapsed")
				return 0
			}
		} else {
			// reset timer
			lp.pvTimer = lp.clock.Now()
		}

		return lp.MinCurrent
	}

	if mode == api.ModePV && !enabled {
		// kick off enable sequence
		if targetCurrent >= lp.MinCurrent ||
			(lp.Enable.Threshold != 0 && sitePower <= lp.Enable.Threshold) {
			lp.log.DEBUG.Printf("site power %.0fW < enable threshold %.0fW", sitePower, lp.Enable.Threshold)

			if lp.pvTimer.IsZero() {
				lp.log.DEBUG.Println("start pv enable timer")
				lp.pvTimer = lp.clock.Now()
			}

			if lp.clock.Since(lp.pvTimer) >= lp.Enable.Delay {
				lp.log.DEBUG.Println("pv enable timer elapsed")
				return lp.MinCurrent
			}
		} else {
			// reset timer
			lp.pvTimer = lp.clock.Now()
		}

		return 0
	}

	// reset timer to disabled state
	lp.log.DEBUG.Printf("pv timer reset")
	lp.pvTimer = time.Time{}

	return targetCurrent
}

// updateChargeMete updates and publishes single meter
func (lp *LoadPoint) updateChargeMeter() {
	err := retry.Do(func() error {
		value, err := lp.chargeMeter.CurrentPower()
		if err != nil {
			return err
		}

		lp.chargePower = value // update value if no error
		lp.log.DEBUG.Printf("charge power: %.1fW", value)
		lp.publish("chargePower", value)

		return nil
	}, retryOptions...)

	if err != nil {
		err = errors.Wrapf(err, "updating charge meter")
		lp.log.ERROR.Printf("%v", err)
	}
}

// chargeDuration returns for how long the charge cycle has been running
func (lp *LoadPoint) chargeDuration() time.Duration {
	d, err := lp.chargeTimer.ChargingTime()
	if err != nil {
		lp.log.ERROR.Printf("charge timer error: %v", err)
		return 0
	}
	return d.Round(time.Second)
}

// chargedEnergy returns energy consumption since charge start in kWh
func (lp *LoadPoint) chargedEnergy() float64 {
	f, err := lp.chargeRater.ChargedEnergy()
	if err != nil {
		lp.log.ERROR.Printf("charge rater error: %v", err)
		return 0
	}
	return f
}

// publish charged energy and duration
func (lp *LoadPoint) publishChargeProgress() {
	lp.publish("chargedEnergy", 1e3*lp.chargedEnergy()) // return Wh for UI
	lp.publish("chargeDuration", lp.chargeDuration())
}

// remainingChargeDuration returns the remaining charge time
func (lp *LoadPoint) remainingChargeDuration(chargePercent float64) time.Duration {
	if !lp.charging {
		return -1
	}

	if lp.chargePower > 0 && lp.vehicle != nil {
		whRemaining := (1 - chargePercent/100.0) * 1e3 * float64(lp.vehicle.Capacity())
		return time.Duration(float64(time.Hour) * whRemaining / lp.chargePower).Round(time.Second)
	}

	return -1
}

// publish state of charge and remaining charge duration
func (lp *LoadPoint) publishSoC() {
	if lp.vehicle == nil {
		return
	}

	if lp.connected() {
		f, err := lp.vehicle.ChargeState()
		if err == nil {
			lp.log.DEBUG.Printf("vehicle soc: %.1f%%", f)
			lp.publish("socCharge", f)
			lp.publish("chargeEstimate", lp.remainingChargeDuration(f))
			return
		}
		lp.log.ERROR.Printf("vehicle error: %v", err)
	}

	lp.publish("socCharge", -1)
	lp.publish("chargeEstimate", -1)
}

// Update is the main control function. It reevaluates meters and charger state
func (lp *LoadPoint) Update(mode api.ChargeMode, sitePower float64) {
	// read and publish meters first
	lp.updateChargeMeter()

	// update ChargeRater here to make sure initial meter update is caught
	lp.bus.Publish(evChargeCurrent, lp.handler.TargetCurrent())
	lp.bus.Publish(evChargePower, lp.chargePower)

	// update progress and soc before status is updated
	lp.publishChargeProgress()
	lp.publishSoC()

	// read and publish status
	if err := retry.Do(lp.updateChargeStatus, retryOptions...); err != nil {
		lp.log.ERROR.Printf("charge controller error: %v", err)
		return
	}

	lp.publish("connected", lp.connected())
	lp.publish("charging", lp.charging)

	// sync settings with charger
	if lp.status != api.StatusA {
		lp.handler.SyncEnabled()
	}

	// phase detection - run only when actually charging
	if lp.charging {
		lp.detectPhases()
	}

	// check if car connected and ready for charging
	var err error

	// execute loading strategy
	switch mode {
	case api.ModeOff:
		err = lp.handler.Ramp(0, true)

	case api.ModeNow:
		// ensure that new connections happen at min current
		current := lp.MinCurrent
		if lp.connected() {
			current = lp.MaxCurrent
		}
		err = lp.handler.Ramp(current, true)

	case api.ModeMinPV, api.ModePV:
		targetCurrent := lp.maxCurrent(mode, sitePower)
		if !lp.connected() {
			// ensure minimum current when not connected
			// https://github.com/andig/evcc/issues/105
			targetCurrent = min(lp.MinCurrent, targetCurrent)
		}
		lp.log.DEBUG.Printf("target charge current: %dA", targetCurrent)

		err = lp.handler.Ramp(targetCurrent)
	}

	if err != nil {
		lp.log.ERROR.Println(err)
	}
}
