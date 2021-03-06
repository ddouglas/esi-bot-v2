package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/eveisesi/eb2/pkg/tools"
	nslack "github.com/nlopes/slack"
	"github.com/nlopes/slack/slackevents"
	"github.com/patrickmn/go-cache"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func (s *server) handlePostSlack(w http.ResponseWriter, r *http.Request) {

	var ctx = r.Context()

	err := verifySlackReqeust(r, s.config.SlackSigningSecret)
	if err != nil {
		s.writeError(ctx, w, err, http.StatusBadRequest)
		return
	}

	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(r.Body)
	if err != nil {
		s.writeError(ctx, w, err, http.StatusInternalServerError)
		return
	}

	body := buf.Bytes()
	event, err := slackevents.ParseEvent(json.RawMessage(body), slackevents.OptionNoVerifyToken())
	if err != nil {
		s.writeError(ctx, w, err, http.StatusInternalServerError)
		return
	}

	if event.Type == slackevents.URLVerification {
		var r *slackevents.ChallengeResponse
		err := json.Unmarshal(body, &r)
		if err != nil {
			s.writeError(ctx, w, err, http.StatusInternalServerError)
			return
		}

		s.writeSuccess(ctx, w, r, http.StatusOK)
		return
	}

	go func(event slackevents.EventsAPIEvent) {
		// Delay for dramatic effect
		switch e := event.InnerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			s.slack.ProcessEvent(ctx, e)
		}
	}(event)

	s.writeSuccess(ctx, w, nil, http.StatusOK)

}

var (
	stateMap = cache.New(time.Minute*5, time.Minute*5)
)

type (
	SlackInvite struct {
		State string `json:"state"`
		Code  string `json:"code"`
	}

	Token struct {
		Token string `json:"token"`
	}
)

func (s SlackInvite) IsValid() bool {
	return (s.State != "" && s.Code != "")
}

func (s *server) handleGetSlackInvite(w http.ResponseWriter, r *http.Request) {

	var ctx = r.Context()

	state := tools.RandomString(16)
	stateMap.Set(state, true, 0)

	query := url.Values{}
	query.Set("response_type", "code")
	query.Set("redirect_uri", s.config.EveCallback)
	query.Set("client_id", s.config.EveClientID)
	query.Set("state", state)

	uri := url.URL{
		Scheme:   "https",
		Host:     "login.eveonline.com",
		Path:     "/v2/oauth/authorize",
		RawQuery: query.Encode(),
	}

	s.writeSuccess(ctx, w, struct {
		Url string `json:"url"`
	}{
		Url: uri.String(),
	}, http.StatusOK)

}

func (s *server) handlePostSlackInvite(w http.ResponseWriter, r *http.Request) {

	var ctx = r.Context()

	var body SlackInvite
	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil {
		s.writeError(ctx, w, err, http.StatusBadRequest)
		return
	}

	if !body.IsValid() {
		s.writeError(ctx, w, errors.New("invalid body received. Please provide both code and state"), http.StatusBadRequest)
		return
	}

	_, found := stateMap.Get(body.State)
	if !found {
		s.writeError(ctx, w, errors.New("invalid state received"), http.StatusBadRequest)
		return
	}

	stateMap.Delete(body.State)

	uri := url.Values{}
	uri.Set("grant_type", "authorization_code")
	uri.Set("code", body.Code)

	req, err := http.NewRequest(http.MethodPost, "https://login.eveonline.com/v2/oauth/token", bytes.NewBuffer([]byte(uri.Encode())))
	if err != nil {
		s.writeError(ctx, w, errors.Wrap(err, "unable to configure post request to ccp"), http.StatusInternalServerError)
		return
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Host", "login.eveonline.com")
	req.Header.Set("User-Agent", "Tweetfleet Slack Inviter (david@onetwentyseven.dev || TF Slack @doubled)")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(s.config.EveClientID+":"+s.config.EveClientSecret)))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.writeError(ctx, w, errors.Wrap(err, "failed to make post request to ccp"), http.StatusInternalServerError)
		return
	}

	var data map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&data)
	if err != nil {
		s.writeError(ctx, w, errors.Wrap(err, "unable to parse response from ccp"), http.StatusInternalServerError)
		return
	}

	s.writeSuccess(ctx, w, data, http.StatusOK)

}

type SlackInviteSend struct {
	Email string `json:"email"`
}

type SlackInvitePayload struct {
	Token    string `json:"token"`
	Email    string `json:"email"`
	RealName string `json:"real_name"`
}

type SlackInviteResponse struct {
	Ok    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *server) handlePostSlackInviteSend(w http.ResponseWriter, r *http.Request) {

	var ctx = r.Context()

	var body SlackInviteSend
	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil {
		s.writeError(ctx, w, errors.Wrap(err, "unable to decode request body"), http.StatusBadRequest)
		return
	}

	if body.Email == "" {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(SlackInviteResponse{
			Ok:    false,
			Error: "email_invalid: please supply a valid, non-empty email address",
		})
	}

	check := ctx.Value(tokenKey)
	if check == nil {
		s.writeError(ctx, w, errors.New("token not found"), http.StatusInternalServerError)
		return
	}

	token := check.(*jwt.Token)

	realName, ok := token.Claims.(jwt.MapClaims)["name"].(string)
	if !ok {
		s.logger.WithFields(logrus.Fields{"token": token.Claims}).Error("Real Name is not a string")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	type message struct {
		Message string `json:"message"`
	}

	msg := fmt.Sprintf("%s (%s) has requested an invitation to Tweetfleet.", realName, body.Email)
	channel, timestamp, err := s.goslack.PostMessage(s.config.SlackModChannel, nslack.MsgOptionText(msg, false))
	if err != nil {
		s.logger.WithError(err).WithFields(logrus.Fields{
			"channel":   channel,
			"timestamp": timestamp,
			"message":   msg,
		}).Error("failed to post success message to mod chat.")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(message{
		Message: "Your request has been submitted successfully. Please monitor your inbox for an invitation for the Tweetfleet Staff. Thank You",
	})
	w.WriteHeader(http.StatusOK)
	return

	// endpoint := "https://slack.com/api/users.admin.invite"

	// uri := url.Values{}
	// uri.Set("token", s.config.SlackLegacyAPIToken)
	// uri.Set("email", body.Email)
	// uri.Set("real_name", realName)

	// resp, err := http.PostForm(endpoint, uri)
	// if err != nil {
	// 	s.writeError(ctx, w, err, http.StatusInternalServerError)
	// 	return
	// }

	// var slackResp = &SlackInviteResponse{}
	// err = json.NewDecoder(resp.Body).Decode(slackResp)
	// if err != nil {
	// 	s.writeError(ctx, w, errors.Wrap(err, "unable to decode response from slack"), http.StatusInternalServerError)
	// 	return
	// }

	// status := http.StatusOK

	// switch slackResp.Ok {
	// case true:

	// case false:
	// 	status = http.StatusBadRequest
	// 	msg := fmt.Sprintf("Uh Oh, I'm having issues inviting %s (%s) to TF Slack. Slack Response Dump: %s", realName, body.Email, slackResp.Error)
	// 	channel, timestamp, err := s.goslack.PostMessage(s.config.SlackModChannel, nslack.MsgOptionText(msg, false))
	// 	if err != nil {
	// 		s.logger.WithError(err).WithFields(logrus.Fields{
	// 			"channel":   channel,
	// 			"timestamp": timestamp,
	// 			"message":   msg,
	// 		}).Error("failed to post message to mod chat.")
	// 	}
	// }

	// data, _ := json.Marshal(slackResp)

	// w.WriteHeader(status)
	// _, _ = w.Write(data)

}

func verifySlackReqeust(req *http.Request, secret string) error {
	verifier, err := nslack.NewSecretsVerifier(req.Header, secret)
	if err != nil {
		return errors.Wrap(err, "failed to create secrets verifier")
	}

	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		return errors.Wrap(err, "failed to read the body from the request")
	}

	req.Body = ioutil.NopCloser(bytes.NewBuffer(body))

	_, err = verifier.Write(body)
	if err != nil {
		return errors.Wrap(err, "failed to write body to the verifier")
	}

	err = verifier.Ensure()
	if err != nil {
		return errors.Wrap(err, "failed to ensure the verifier")
	}

	return nil
}
