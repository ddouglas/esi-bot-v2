package slack

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/eveisesi/eb2"
	nslack "github.com/nlopes/slack"
	"github.com/patrickmn/go-cache"
	"github.com/sirupsen/logrus"
)

type StatusCategory struct {
	Status string
	Emoji  string
	Color  string
}

var categories = []StatusCategory{
	{
		Status: "red",
		Emoji:  ":fire:",
		Color:  "danger",
	},
	{
		Status: "yellow",
		Emoji:  ":fire_engine:",
		Color:  "warning",
	},
}

var statusCache = cache.New(time.Minute*1, time.Second*30)

func (s *service) makeEveServerStatusMessage(event Event) {

	uri, _ := url.Parse(eb2.ESI_BASE)
	uri.Path = "/v1/status"

	resp, err := http.Get(uri.String())
	if err != nil {
		_, _, _ = s.goslack.PostMessage(event.origin.Channel, nslack.MsgOptionText(err.Error(), true))
		return
	}
	defer resp.Body.Close()
	var attachment nslack.Attachment
	if resp.StatusCode > 200 {

		if resp.StatusCode != 503 {
			indeterminate := "Cannet Determine server status. It might be offline, or experiencing connectivity issues."
			attachment = nslack.Attachment{
				Color: "danger",
				// Leaving this like this so that we can support other servers in the future
				Title:    fmt.Sprintf("%s status", "Tranquility"),
				Text:     indeterminate,
				Fallback: fmt.Sprintf("%s Status: %s", "Tranquility", indeterminate),
			}

		} else {
			attachment = nslack.Attachment{
				Color: "danger",
				// Leaving this like this so that we can support other servers in the future
				Title:    fmt.Sprintf("%s status", "Tranquility"),
				Text:     "Offline",
				Fallback: fmt.Sprintf("%s Status: Offline", "Tranquility"),
			}
		}

		s.logger.Info("Responding to request for eve server status")
		channel, timestamp, err := s.goslack.PostMessage(event.origin.Channel, nslack.MsgOptionAttachments(attachment))
		if err != nil {
			s.logger.WithError(err).Error("failed to respond to request for eve server status.")
			return
		}
		s.logger.WithFields(logrus.Fields{
			"channel":   channel,
			"timestamp": timestamp,
		}).Info("successfully responded to request for eve server status")
		return

	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		_, _, _ = s.goslack.PostMessage(event.origin.Channel, nslack.MsgOptionText(err.Error(), true))
		return
	}
	var status eb2.ServerStatus
	err = json.Unmarshal(data, &status)
	if err != nil {
		_, _, _ = s.goslack.PostMessage(event.origin.Channel, nslack.MsgOptionText(err.Error(), true))
		return
	}
	color := "good"
	inVip := ""
	if status.Vip {
		color = "warning"
		inVip = ", in VIP"
	}
	attachment = nslack.Attachment{
		Color: color,
		Title: fmt.Sprintf("%s status", "Tranquility"),
		Fields: []nslack.AttachmentField{
			nslack.AttachmentField{
				Title: "Players Online",
				Value: fmt.Sprintf("%d", status.Players),
			},
			nslack.AttachmentField{
				Title: "Started At",
				Value: status.StartTime.Format(layoutESI),
				Short: true,
			},
			nslack.AttachmentField{
				Title: "Running For",
				Value: determineServerRunTime(status.StartTime),
				Short: true,
			},
		},
		Fallback: fmt.Sprintf("%s status: %d player online, started at %s%s", "Tranquility", status.Players, status.StartTime.Format(layoutESI), inVip),
	}

	s.logger.Info("Responding to request for eve server status")
	channel, timestamp, err := s.goslack.PostMessage(event.origin.Channel, nslack.MsgOptionAttachments(attachment))
	if err != nil {
		s.logger.WithError(err).Error("failed to respond to request for eve server status.")
		return
	}
	s.logger.WithFields(logrus.Fields{
		"channel":   channel,
		"timestamp": timestamp,
	}).Info("successfully responded to request for eve server status")
}

func determineServerRunTime(from time.Time) string {

	n := time.Since(from)
	return time.Time{}.Add(n).Format("15h 04m 05s")

}

func fetchRouteStatuses(version string) (routes []*eb2.ESIStatus, err error) {

	uri, _ := url.Parse(eb2.ESI_BASE)
	uri.Path = "status.json"

	query := url.Values{}
	query.Set("version", version)

	uri.RawQuery = query.Encode()

	resp, err := http.Get(uri.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(data, &routes)
	if err != nil {
		return nil, err
	}

	statusCache.Set(version, routes, 0)

	return

}

func (s *service) makeESIStatusMessage(event Event) {

	version := "latest"
	if _, ok := event.flags["version"]; ok {
		version = event.flags["version"]
	}

	routes, found := checkCache(version)
	if !found {

		routes, err := fetchRouteStatuses(version)
		if err != nil {
			_, _, _ = s.goslack.PostMessage(event.origin.Channel, nslack.MsgOptionText(err.Error(), true))
			return
		}

		statusCache.Flush()
		statusCache.Set(version, routes, 0)

	}

	var attachments []nslack.Attachment
	for _, category := range categories {
		categoryRoutes := []*eb2.ESIStatus{}
		for _, route := range routes {
			if route.Status == category.Status {
				categoryRoutes = append(categoryRoutes, route)
			}
		}

		if len(categoryRoutes) > 0 {

			attachment := nslack.Attachment{
				Color: category.Color,
				Fallback: fmt.Sprintf(
					"%s: %d out of %d, %.3f%%",
					strings.Title(category.Status),
					len(categoryRoutes),
					len(routes),
					percentage(len(categoryRoutes), len(routes)),
				),
				Text: fmt.Sprintf(
					"%s %s %s %s",
					category.Emoji,
					fmt.Sprintf(
						"%d %s (out of %d,  %.3f%%)",
						len(categoryRoutes),
						strings.Title(category.Status),
						len(routes),
						percentage(len(categoryRoutes), len(routes)),
					),
					category.Emoji,
					generateRoutesString(categoryRoutes),
				),
			}
			attachments = append(attachments, attachment)
		}

	}
	if len(attachments) == 0 {
		attachments = append(attachments, nslack.Attachment{
			Text: ":ok_hand:",
		})
	}

	s.logger.Info("Responding to request for esi route status.")
	channel, timestamp, err := s.goslack.PostMessage(event.origin.Channel, nslack.MsgOptionAttachments(attachments...))
	if err != nil {
		s.logger.WithError(err).Error("failed to respond to request for esi route status.")
		return
	}
	s.logger.WithFields(logrus.Fields{
		"channel":   channel,
		"timestamp": timestamp,
	}).Info("successfully responded to request for esi route status.")

}

func checkCache(version string) ([]*eb2.ESIStatus, bool) {
	check, found := statusCache.Get(version)
	if found {
		return check.([]*eb2.ESIStatus), true
	}
	return nil, false
}

func percentage(top int, bottom int) float64 {
	if bottom == 0 {
		return 0.00
	}
	return 1 - ((float64(top) / float64(bottom)) * 100)
}

func generateRoutesString(routes []*eb2.ESIStatus) string {

	if len(routes) == 0 {
		return ""
	}

	t := []string{}

	processed := 0
	for i, route := range routes {
		processed++
		t = append(t, fmt.Sprintf(
			"%s %s", strings.ToUpper(route.Method), route.Route,
		))
		if processed > 50 {
			t = append(t, fmt.Sprintf(
				"%d", len(routes[i:]),
			))
			break
		}

	}

	return fmt.Sprintf("```%s```", strings.Join(t, "\n"))
}