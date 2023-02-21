package gomeassistant

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/golang-module/carbon"
	"github.com/gorilla/websocket"
	sunriseLib "github.com/nathan-osman/go-sunrise"
	"saml.dev/gome-assistant/internal"
	"saml.dev/gome-assistant/internal/http"
	pq "saml.dev/gome-assistant/internal/priorityqueue"
	ws "saml.dev/gome-assistant/internal/websocket"
)

type App struct {
	ctx        context.Context
	ctxCancel  context.CancelFunc
	conn       *websocket.Conn
	httpClient *http.HttpClient

	service *Service
	state   *State

	schedules         pq.PriorityQueue
	intervals         pq.PriorityQueue
	entityListeners   map[string][]*EntityListener
	entityListenersId int64
	eventListeners    map[string][]*EventListener
}

/*
DurationString represents a duration, such as "2s" or "24h".
See https://pkg.go.dev/time#ParseDuration for all valid time units.
*/
type DurationString string

/*
TimeString is a 24-hr format time "HH:MM" such as "07:30".
*/
type TimeString string

type timeRange struct {
	start time.Time
	end   time.Time
}

type NewAppRequest struct {
	// Required
	// IpAddress of your Home Assistant instance i.e. "localhost"
	// or "192.168.86.59" etc.
	IpAddress string

	// Optional
	// Port number Home Assistant is running on. Defaults to 8123.
	Port string

	// Required
	// Auth token generated in Home Assistant. Used
	// to connect to the Websocket API.
	HAAuthToken string

	// Required
	// EntityId of the zone representing your home e.g. "zone.home".
	// Used to pull latitude/longitude from Home Assistant
	// to calculate sunset/sunrise times.
	HomeZoneEntityId string
}

/*
NewApp establishes the websocket connection and returns an object
you can use to register schedules and listeners.
*/
func NewApp(request NewAppRequest) *App {
	if request.IpAddress == "" || request.HAAuthToken == "" || request.HomeZoneEntityId == "" {
		log.Fatalln("IpAddress, HAAuthToken, and HomeZoneEntityId are all required arguments in NewAppRequest.")
	}
	port := request.Port
	if port == "" {
		port = "8123"
	}
	conn, ctx, ctxCancel := ws.SetupConnection(request.IpAddress, port, request.HAAuthToken)

	httpClient := http.NewHttpClient(request.IpAddress, port, request.HAAuthToken)

	service := newService(conn, ctx, httpClient)
	state := newState(httpClient, request.HomeZoneEntityId)

	return &App{
		conn:            conn,
		ctx:             ctx,
		ctxCancel:       ctxCancel,
		httpClient:      httpClient,
		service:         service,
		state:           state,
		schedules:       pq.New(),
		intervals:       pq.New(),
		entityListeners: map[string][]*EntityListener{},
		eventListeners:  map[string][]*EventListener{},
	}
}

func (a *App) Cleanup() {
	if a.ctxCancel != nil {
		a.ctxCancel()
	}
}

func (a *App) RegisterSchedules(schedules ...DailySchedule) {
	for _, s := range schedules {
		// realStartTime already set for sunset/sunrise
		if s.isSunrise || s.isSunset {
			s.nextRunTime = getNextSunRiseOrSet(a, s.isSunrise, s.sunOffset).Carbon2Time()
			a.schedules.Insert(s, float64(s.nextRunTime.Unix()))
			continue
		}

		now := carbon.Now()
		startTime := carbon.Now().SetTimeMilli(s.hour, s.minute, 0, 0)

		// advance first scheduled time by frequency until it is in the future
		if startTime.Lt(now) {
			startTime = startTime.AddDay()
		}

		s.nextRunTime = startTime.Carbon2Time()
		a.schedules.Insert(s, float64(startTime.Carbon2Time().Unix()))
	}
}

func (a *App) RegisterIntervals(intervals ...Interval) {
	for _, i := range intervals {
		if i.frequency == 0 {
			log.Fatalf("A schedule must use either set frequency via Every().\n")
		}

		i.nextRunTime = internal.ParseTime(string(i.startTime)).Carbon2Time()
		now := time.Now()
		for i.nextRunTime.Before(now) {
			i.nextRunTime = i.nextRunTime.Add(i.frequency)
		}
		a.intervals.Insert(i, float64(i.nextRunTime.Unix()))
	}
}

func (a *App) RegisterEntityListeners(etls ...EntityListener) {
	for _, etl := range etls {
		etl := etl
		if etl.delay != 0 && etl.toState == "" {
			log.Fatalln("EntityListener error: you have to use ToState() when using Duration()")
		}

		for _, entity := range etl.entityIds {
			if elList, ok := a.entityListeners[entity]; ok {
				a.entityListeners[entity] = append(elList, &etl)
			} else {
				a.entityListeners[entity] = []*EntityListener{&etl}
			}
		}
	}
}

func (a *App) RegisterEventListeners(evls ...EventListener) {
	for _, evl := range evls {
		evl := evl
		for _, eventType := range evl.eventTypes {
			if elList, ok := a.eventListeners[eventType]; ok {
				a.eventListeners[eventType] = append(elList, &evl)
			} else {
				ws.SubscribeToEventType(eventType, a.conn, a.ctx)
				a.eventListeners[eventType] = []*EventListener{&evl}
			}
		}
	}
}

func (a *App) GetService() *Service {
	return a.service
}

func getSunriseSunset(s *State, sunrise bool, dateToUse carbon.Carbon, offset ...DurationString) carbon.Carbon {
	date := dateToUse.Carbon2Time()
	rise, set := sunriseLib.SunriseSunset(s.latitude, s.longitude, date.Year(), date.Month(), date.Day())
	rise, set = rise.Local(), set.Local()

	val := set
	printString := "Sunset"
	if sunrise {
		val = rise
		printString = "Sunrise"
	}

	setOrRiseToday := carbon.Parse(val.String())

	var t time.Duration
	var err error
	if len(offset) == 1 {
		t, err = time.ParseDuration(string(offset[0]))
		if err != nil {
			log.Fatalf(fmt.Sprintf("Could not parse offset passed to %s: \"%s\"\n", printString, offset[0]))
		}
	}

	// add offset if set, this code works for negative values too
	if t.Microseconds() != 0 {
		setOrRiseToday = setOrRiseToday.AddMinutes(int(t.Minutes()))
	}

	return setOrRiseToday
}

func getNextSunRiseOrSet(a *App, sunrise bool, offset ...DurationString) carbon.Carbon {
	sunriseOrSunset := getSunriseSunset(a.state, sunrise, carbon.Now(), offset...)
	if sunriseOrSunset.Lt(carbon.Now()) {
		// if we're past today's sunset or sunrise (accounting for offset) then get tomorrows
		// as that's the next time the schedule will run
		sunriseOrSunset = getSunriseSunset(a.state, sunrise, carbon.Tomorrow(), offset...)
	}
	return sunriseOrSunset
}

func (a *App) Start() {
	log.Default().Println("Starting", a.schedules.Len(), "schedules")
	log.Default().Println("Starting", len(a.entityListeners), "entity listeners")
	log.Default().Println("Starting", len(a.eventListeners), "event listeners")

	go runSchedules(a)
	go runIntervals(a)

	// subscribe to state_changed events
	id := internal.GetId()
	ws.SubscribeToStateChangedEvents(id, a.conn, a.ctx)
	a.entityListenersId = id

	// entity listeners runOnStartup
	for eid, etls := range a.entityListeners {
		for _, etl := range etls {
			// ensure each ETL only runs once, even if
			// it listens to multiple entities
			if etl.runOnStartup && !etl.runOnStartupCompleted {
				entityState, err := a.state.Get(eid)
				if err != nil {
					log.Default().Println("Failed to get entity state \"", eid, "\" during startup, skipping RunOnStartup")
				}

				etl.runOnStartupCompleted = true
				go etl.callback(a.service, a.state, EntityData{
					TriggerEntityId: eid,
					FromState:       entityState.State,
					FromAttributes:  entityState.Attributes,
					ToState:         entityState.State,
					ToAttributes:    entityState.Attributes,
					LastChanged:     entityState.LastChanged,
				})
			}
		}
	}

	// entity listeners and event listeners
	elChan := make(chan ws.ChanMsg)
	go ws.ListenWebsocket(a.conn, a.ctx, elChan)
	var msg ws.ChanMsg
	for {
		msg = <-elChan
		if a.entityListenersId == msg.Id {
			go callEntityListeners(a, msg.Raw)
		} else {
			go callEventListeners(a, msg)
		}
	}
}
