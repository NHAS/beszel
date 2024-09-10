// Package alerts handles alert management and delivery.
package alerts

import (
	"beszel/internal/entities/system"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"os"

	"github.com/containrrr/shoutrrr"
	"github.com/labstack/echo/v5"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/models"
	"github.com/pocketbase/pocketbase/tools/mailer"
)

type AlertData struct {
	User    *models.Record
	Title   string
	Message string
	Link    string
}

type AlertManager struct {
	app *pocketbase.PocketBase
}

func NewAlertManager(app *pocketbase.PocketBase) *AlertManager {
	am := &AlertManager{
		app: app,
	}

	// err := am.sendShoutrrrAlert(&mailer.Message{
	// 	Subject: "Testing shoutrrr",
	// 	Text:    "this is a test from beszel",
	// })
	// if err != nil {
	// 	log.Println("Error sending shoutrrr alert", "err", err.Error())
	// }

	return am
}

func (am *AlertManager) HandleSystemAlerts(newStatus string, newRecord *models.Record, oldRecord *models.Record) {
	alertRecords, err := am.app.Dao().FindRecordsByExpr("alerts",
		dbx.NewExp("system = {:system}", dbx.Params{"system": oldRecord.GetId()}),
	)
	if err != nil || len(alertRecords) == 0 {
		// log.Println("no alerts found for system")
		return
	}
	// log.Println("found alerts", len(alertRecords))
	var systemInfo *system.Info
	for _, alertRecord := range alertRecords {
		name := alertRecord.GetString("name")
		switch name {
		case "Status":
			am.handleStatusAlerts(newStatus, oldRecord, alertRecord)
		case "CPU", "Memory", "Disk":
			if newStatus != "up" {
				continue
			}
			if systemInfo == nil {
				systemInfo = getSystemInfo(newRecord)
			}
			if name == "CPU" {
				am.handleSlidingValueAlert(newRecord, alertRecord, name, systemInfo.Cpu)
			} else if name == "Memory" {
				am.handleSlidingValueAlert(newRecord, alertRecord, name, systemInfo.MemPct)
			} else if name == "Disk" {
				am.handleSlidingValueAlert(newRecord, alertRecord, name, systemInfo.DiskPct)
			}
		}
	}
}

func getSystemInfo(record *models.Record) *system.Info {
	var SystemInfo system.Info
	record.UnmarshalJSONField("info", &SystemInfo)
	return &SystemInfo
}

func (am *AlertManager) handleSlidingValueAlert(newRecord *models.Record, alertRecord *models.Record, name string, curValue float64) {
	triggered := alertRecord.GetBool("triggered")
	threshold := alertRecord.GetFloat("value")
	// fmt.Println(name, curValue, "threshold", threshold, "triggered", triggered)
	var subject string
	var body string
	var systemName string
	if !triggered && curValue > threshold {
		alertRecord.Set("triggered", true)
		systemName = newRecord.GetString("name")
		subject = fmt.Sprintf("%s usage above threshold on %s", name, systemName)
		body = fmt.Sprintf("%s usage on %s is %.1f%%.", name, systemName, curValue)
	} else if triggered && curValue <= threshold {
		alertRecord.Set("triggered", false)
		systemName = newRecord.GetString("name")
		subject = fmt.Sprintf("%s usage below threshold on %s", name, systemName)
		body = fmt.Sprintf("%s usage on %s is below threshold at %.1f%%.", name, systemName, curValue)
	} else {
		// fmt.Println(name, "not triggered")
		return
	}
	if err := am.app.Dao().SaveRecord(alertRecord); err != nil {
		// app.Logger().Error("failed to save alert record", "err", err.Error())
		return
	}
	// expand the user relation and send the alert
	if errs := am.app.Dao().ExpandRecord(alertRecord, []string{"user"}, nil); len(errs) > 0 {
		// app.Logger().Error("failed to expand user relation", "errs", errs)
		return
	}
	if user := alertRecord.ExpandedOne("user"); user != nil {
		// am.sendAlert(&mailer.Message{
		// 	To:      []mail.Address{{Address: user.GetString("email")}},
		// 	Subject: subject,
		// 	Text:    body,
		// })
		am.sendAlert(AlertData{
			User:    user,
			Title:   subject,
			Message: body,
			Link:    am.app.Settings().Meta.AppUrl + "/system/" + url.QueryEscape(systemName),
		})
	}
}

func (am *AlertManager) handleStatusAlerts(newStatus string, oldRecord *models.Record, alertRecord *models.Record) error {
	var alertStatus string
	switch newStatus {
	case "up":
		if oldRecord.GetString("status") == "down" {
			alertStatus = "up"
		}
	case "down":
		if oldRecord.GetString("status") == "up" {
			alertStatus = "down"
		}
	}
	if alertStatus == "" {
		return nil
	}
	// expand the user relation
	if errs := am.app.Dao().ExpandRecord(alertRecord, []string{"user"}, nil); len(errs) > 0 {
		return fmt.Errorf("failed to expand: %v", errs)
	}
	user := alertRecord.ExpandedOne("user")
	if user == nil {
		return nil
	}
	emoji := "\U0001F534"
	if alertStatus == "up" {
		emoji = "\u2705"
	}
	// send alert
	systemName := oldRecord.GetString("name")
	am.sendAlert(AlertData{
		User:    user,
		Title:   fmt.Sprintf("Connection to %s is %s %v", systemName, alertStatus, emoji),
		Message: fmt.Sprintf("Connection to %s is %s", systemName, alertStatus),
		Link:    am.app.Settings().Meta.AppUrl + "/system/" + url.QueryEscape(systemName),
	})
	return nil
}

func (am *AlertManager) sendAlert(data AlertData) {
	shoutrrrUrl := os.Getenv("SHOUTRRR_URL")
	if shoutrrrUrl != "" {
		err := am.SendShoutrrrAlert(shoutrrrUrl, data.Title, data.Message, data.Link)
		if err == nil {
			log.Println("Sent shoutrrr alert")
			return
		}
		log.Println("Failed to send alert via shoutrrr, falling back to email notification. ", "err", err.Error())
	}
	// todo: email enable / disable and testing
	message := mailer.Message{
		To:      []mail.Address{{Address: data.User.GetString("email")}},
		Subject: data.Title,
		Text:    data.Message + fmt.Sprintf("\n\n%s", data.Link),
		From: mail.Address{
			Address: am.app.Settings().Meta.SenderAddress,
			Name:    am.app.Settings().Meta.SenderName,
		},
	}
	log.Println("Sending alert via email")
	if err := am.app.NewMailClient().Send(&message); err != nil {
		am.app.Logger().Error("Failed to send alert: ", "err", err.Error())
	} else {
		am.app.Logger().Info("Sent email alert", "to", message.To, "subj", message.Subject)
	}
}

func (am *AlertManager) SendShoutrrrAlert(notificationUrl string, title string, message string, link string) error {
	supportsTitle := []string{"bark", "discord", "gotify", "ifttt", "join", "matrix", "ntfy", "opsgenie", "pushbullet", "pushover", "slack", "teams", "telegram", "zulip"}
	supportsLink := []string{"ntfy"}
	// Parse the URL
	parsedURL, err := url.Parse(notificationUrl)
	if err != nil {
		return fmt.Errorf("error parsing URL: %v", err)
	}

	scheme := parsedURL.Scheme

	// // Get query parameters
	queryParams := parsedURL.Query()

	// Add title
	if !sliceContains(supportsTitle, scheme) {
		message = title + "\n\n" + message
	} else {
		queryParams.Add("title", title)

	}
	// Add link
	if !sliceContains(supportsLink, scheme) {
		// add link to the message
		message += "\n\n" + link
	} else {
		// ntfy link
		if scheme == "ntfy" {
			queryParams.Add("Actions", fmt.Sprintf("view, Open Beszel, %s", am.app.Settings().Meta.AppUrl))
		}
	}

	//
	if scheme == "generic" {
		queryParams.Add("template", "json")
		queryParams.Add("$title", title)
	}

	// Encode the modified query parameters back into the URL
	parsedURL.RawQuery = queryParams.Encode()
	log.Println("URL after modification:", parsedURL.String())

	err = shoutrrr.Send(parsedURL.String(), message)

	if err == nil {
		am.app.Logger().Info("Sent shoutrrr alert", "title", title)
	} else {
		am.app.Logger().Error("Error sending shoutrrr alert", "errs", err)
		return err
	}
	return nil
}

// Contains checks if a string is present in a slice of strings
func sliceContains(slice []string, item string) bool {
	for _, v := range slice {
		if v == item {
			return true
		}
	}
	return false
}

func (am *AlertManager) SendTestNotification(c echo.Context) error {
	requestData := apis.RequestInfo(c)
	if requestData.AuthRecord == nil {
		return apis.NewForbiddenError("Forbidden", nil)
	}
	url := c.QueryParam("url")
	log.Println("url", url)
	if url == "" {
		return c.JSON(http.StatusOK, map[string]string{"err": "URL is required"})
	}
	err := am.SendShoutrrrAlert(url, "Test Alert", "This is a notification from Beszel.", am.app.Settings().Meta.AppUrl)
	if err != nil {
		return c.JSON(http.StatusOK, map[string]string{"err": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]bool{"err": false})
}
