package main

import (
	"bytes"
	"expvar"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"text/template"
	"time"

	"github.com/go-recaptcha/recaptcha"
	"github.com/gorilla/handlers"
	"github.com/kelseyhightower/envconfig"
	"github.com/nlopes/slack"
	"github.com/paulbellamy/ratecounter"
)

var indexTemplate = template.Must(template.New("index.tmpl").ParseFiles("templates/index.tmpl"))
var badgeTemplate = template.Must(template.New("badge.tmpl").ParseFiles("templates/badge.tmpl"))

var (
	api     *slack.Client
	captcha *recaptcha.Recaptcha
	counter *ratecounter.RateCounter

	ourTeam = new(team)

	m *expvar.Map
	hitsPerMinute,
	requests,
	inviteErrors,
	missingFirstName,
	missingLastName,
	missingEmail,
	missingCoC,
	successfulCaptcha,
	failedCaptcha,
	invalidCaptcha,
	successfulInvites,
	userCount,
	activeUserCount expvar.Int
)

var c Specification

// Specification is the config struct
type Specification struct {
	Port           string `envconfig:"PORT" required:"true"`
	CaptchaSitekey string `required:"true"`
	CaptchaSecret  string `required:"true"`
	SlackToken     string `required:"true"`
	EnforceHTTPS   bool
}

func init() {
	err := envconfig.Process("slackinviter", &c)
	if err != nil {
		log.Fatal(err.Error())
	}
	counter = ratecounter.NewRateCounter(1 * time.Minute)
	m = expvar.NewMap("metrics")
	m.Set("hits_per_minute", &hitsPerMinute)
	m.Set("requests", &requests)
	m.Set("invite_errors", &inviteErrors)
	m.Set("missing_first_name", &missingFirstName)
	m.Set("missing_last_name", &missingLastName)
	m.Set("missing_email", &missingEmail)
	m.Set("missing_coc", &missingCoC)
	m.Set("failed_captcha", &failedCaptcha)
	m.Set("invalid_captcha", &invalidCaptcha)
	m.Set("successful_captcha", &successfulCaptcha)
	m.Set("successful_invites", &successfulInvites)
	m.Set("active_user_count", &activeUserCount)
	m.Set("user_count", &userCount)

	captcha = recaptcha.New(c.CaptchaSecret)
	api = slack.New(c.SlackToken)
}

func main() {
	go pollSlack()
	mux := http.NewServeMux()
	mux.HandleFunc("/invite/", handleInvite)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))
	mux.HandleFunc("/", enforceHTTPSFunc(homepage))
	mux.HandleFunc("/badge.svg", enforceHTTPSFunc(badge))
	mux.Handle("/debug/vars", http.DefaultServeMux)
	err := http.ListenAndServe(":"+c.Port, handlers.CombinedLoggingHandler(os.Stdout, mux))
	if err != nil {
		log.Fatal(err.Error())
	}
}

func enforceHTTPSFunc(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if xfp := r.Header.Get("X-Forwarded-Proto"); c.EnforceHTTPS && xfp == "http" {
			u := *r.URL
			u.Scheme = "https"
			if u.Host == "" {
				u.Host = r.Host
			}
			http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
			return
		}
		h(w, r)
	}
}

// Updates the globals from the slack API
// returns the length of time to sleep before the function
// should be called again
func updateFromSlack() time.Duration {
	users, err := api.GetUsers()
	if err != nil {
		log.Println("error polling slack for users:", err)
		return time.Minute
	}
	var uCount, aCount int64 // users and active users
	for _, u := range users {
		if u.ID != "USLACKBOT" && !u.IsBot && !u.Deleted {
			uCount++
			if u.Presence == "active" {
				aCount++
			}
		}
	}
	userCount.Set(uCount)
	activeUserCount.Set(aCount)

	st, err := api.GetTeamInfo()
	if err != nil {
		log.Println("error polling slack for team info:", err)
		return time.Minute
	}
	ourTeam.Update(st)
	return 10 * time.Minute
}

// pollSlack over and over again
func pollSlack() {
	for {
		time.Sleep(updateFromSlack())
	}
}

//Badge renders the sv badge
func badge(w http.ResponseWriter, r *http.Request) {
	leftText := "slack"
	color := "#E01563"
	padding := 8
	middleSeparator := 4
	charSpacing := 7

	rightText := fmt.Sprintf("%v/%v", strconv.Itoa(int(activeUserCount.Value())), strconv.Itoa(int(userCount.Value())))

	var noActiveText string

	if userCount.Value() > int64(0) {
		noActiveText = fmt.Sprintf("%v", strconv.Itoa(int(userCount.Value())))
	} else {
		noActiveText = "-"
	}

	if activeUserCount.Value() == int64(0) {
		rightText = noActiveText
	}

	leftWidth := padding + (charSpacing * len(leftText)) + middleSeparator
	rightWidth := middleSeparator + (charSpacing * len(rightText)) + padding
	totalWidth := leftWidth + rightWidth
	leftTextWidth := leftWidth / 2
	leftTextHeight := 14
	rightTextWidth := leftWidth + rightWidth/2
	rightTextHeight := 14
	leftTextHeightPlusOne := leftTextHeight + 1
	rightTextHeightPlusOne := rightTextHeight + 1

	var buf bytes.Buffer
	err := badgeTemplate.Execute(
		&buf,
		struct {
			TotalWidth             string
			LeftWidth              string
			RightWidth             string
			MiddleSeparator        string
			Color                  string
			LeftTextWidth          string
			LeftTextHeight         string
			LeftTextHeightPlusOne  string
			RightTextWidth         string
			RightTextHeight        string
			RightTextHeightPlusOne string
			RightText              string
			LeftText               string
			UserCount              string
			ActiveCount            string
		}{
			strconv.Itoa(totalWidth),
			strconv.Itoa(leftWidth),
			strconv.Itoa(rightWidth),
			strconv.Itoa(middleSeparator),
			color,
			strconv.Itoa(leftTextWidth),
			strconv.Itoa(leftTextHeight),
			strconv.Itoa(leftTextHeightPlusOne),
			strconv.Itoa(rightTextWidth),
			strconv.Itoa(rightTextHeight),
			strconv.Itoa(rightTextHeightPlusOne),
			rightText,
			leftText,
			userCount.String(),
			activeUserCount.String(),
		},
	)
	if err != nil {
		log.Println("error rendering template:", err)
		http.Error(w, "error rendering template :-(", http.StatusInternalServerError)
		return
	}
	// Set the header and write the buffer to the http.ResponseWriter
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	buf.WriteTo(w)
}

// Homepage renders the homepage
func homepage(w http.ResponseWriter, r *http.Request) {
	counter.Incr(1)
	hitsPerMinute.Set(counter.Rate())
	requests.Add(1)

	var buf bytes.Buffer
	err := indexTemplate.Execute(
		&buf,
		struct {
			SiteKey,
			UserCount,
			ActiveCount string
			Team *team
		}{
			c.CaptchaSitekey,
			userCount.String(),
			activeUserCount.String(),
			ourTeam,
		},
	)
	if err != nil {
		log.Println("error rendering template:", err)
		http.Error(w, "error rendering template :-(", http.StatusInternalServerError)
		return
	}
	// Set the header and write the buffer to the http.ResponseWriter
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

// ShowPost renders a single post
func handleInvite(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	captchaResponse := r.FormValue("g-recaptcha-response")
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		failedCaptcha.Add(1)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	valid, err := captcha.Verify(captchaResponse, remoteIP)
	if err != nil {
		failedCaptcha.Add(1)
		http.Error(w, "Error validating recaptcha.. Did you click it?", http.StatusPreconditionFailed)
		return
	}
	if !valid {
		invalidCaptcha.Add(1)
		http.Error(w, "Invalid recaptcha", http.StatusInternalServerError)
		return

	}
	successfulCaptcha.Add(1)
	fname := r.FormValue("fname")
	lname := r.FormValue("lname")
	email := r.FormValue("email")
	coc := r.FormValue("coc")
	if email == "" {
		missingEmail.Add(1)
		http.Error(w, "Missing email", http.StatusPreconditionFailed)
		return
	}
	if fname == "" {
		missingFirstName.Add(1)
		http.Error(w, "Missing first name", http.StatusPreconditionFailed)
		return
	}
	if lname == "" {
		missingLastName.Add(1)
		http.Error(w, "Missing last name", http.StatusPreconditionFailed)
		return
	}
	if coc != "1" {
		missingCoC.Add(1)
		http.Error(w, "You need to accept the code of conduct", http.StatusPreconditionFailed)
		return
	}
	err = api.InviteToTeam("Gophers", fname, lname, email)
	if err != nil {
		log.Println("InviteToTeam error:", err)
		inviteErrors.Add(1)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	successfulInvites.Add(1)
}
