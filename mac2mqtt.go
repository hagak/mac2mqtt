package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v2"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

const (
	configPath     = "/Users/jeff/mac2mqtt/mac2mqtt.yaml"
	commandTimeout = 10 * time.Second
	betterDisplay  = "http://localhost:55777"
)

var hostname string
var cfg config

// BetterDisplay's HTTP API; without a timeout a hung request would block the poller forever.
var httpClient = &http.Client{Timeout: 5 * time.Second}

type config struct {
	Ip              string `yaml:"mqtt_ip"`
	Port            string `yaml:"mqtt_port"`
	User            string `yaml:"mqtt_user"`
	Password        string `yaml:"mqtt_password"`
	Hostname        string `yaml:"hostname"`
	DiscoveryPrefix string `yaml:"discovery_prefix"`
}

func (c *config) getConfig() *config {

	configContent, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatal("No config file provided")
	}

	err = yaml.Unmarshal(configContent, c)
	if err != nil {
		log.Fatal("No data in config file")
	}

	if c.Ip == "" {
		log.Fatal("Must specify mqtt_ip in mac2mqtt.yaml")
	}

	if c.Port == "" {
		log.Fatal("Must specify mqtt_port in mac2mqtt.yaml")
	}

	if c.User == "" {
		log.Fatal("Must specify mqtt_user in mac2mqtt.yaml")
	}

	if c.Password == "" {
		log.Fatal("Must specify mqtt_password in mac2mqtt.yaml")
	}
	if c.Hostname == "" {
		c.Hostname = getHostname()
	}
	if c.DiscoveryPrefix == "" {
		c.DiscoveryPrefix = "homeassistant"
	}
	return c
}

func getSerialnumber() string {

	output, err := runShell("/usr/sbin/ioreg -l | /usr/bin/grep IOPlatformSerialNumber")
	if err != nil {
		log.Fatal(err)
	}
	last := output[strings.LastIndex(output, " ")+1:]
	// remove all symbols, but [a-zA-Z0-9_-]
	reg := regexp.MustCompile("[^a-zA-Z0-9_-]+")
	return reg.ReplaceAllString(last, "")
}

func getModel() string {

	output, err := runShell("/usr/sbin/system_profiler SPHardwareDataType |/usr/bin/grep Chip | /usr/bin/sed 's/\\(^.*: \\)\\(.*\\)/\\2/'")
	if err != nil {
		log.Fatal(err)
	}
	return output
}

func getHostname() string {

	hostname, err := os.Hostname()

	if err != nil {
		log.Fatal(err)
	}

	// "name.local" => "name"
	firstPart := strings.Split(hostname, ".")[0]

	// remove all symbols, but [a-zA-Z0-9_-]
	reg := regexp.MustCompile("[^a-zA-Z0-9_-]+")
	return reg.ReplaceAllString(firstPart, "")
}

// getCommandOutput runs a command with a timeout and returns its trimmed stdout.
// Errors are returned to the caller instead of killing the process.
func getCommandOutput(name string, arg ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	stdout, err := exec.CommandContext(ctx, name, arg...).Output()
	out := strings.TrimSuffix(string(stdout), "\n")
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("%s timed out after %s", name, commandTimeout)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return out, fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return out, fmt.Errorf("%s: %v", name, err)
	}
	return out, nil
}

func runShell(cmd string) (string, error) {
	return getCommandOutput("/bin/sh", "-c", cmd)
}

// runCommand runs a command and logs (not exits on) failure, so a failed action from HA is visible in the log.
func runCommand(name string, arg ...string) {
	if _, err := getCommandOutput(name, arg...); err != nil {
		log.Println("Command failed: " + err.Error())
	}
}

func getCaffeinateStatus() bool {
	// grep exits 1 when nothing matches, which just means "not running"
	output, _ := runShell("/bin/ps ax | /usr/bin/grep caffeinate | /usr/bin/grep -v grep")
	return output != ""
}

// currentDisplay returns the BetterDisplay device name for the current audio output,
// or "" if the output isn't one of the Dell monitors.
func currentDisplay() (string, error) {
	source, err := getCommandOutput("/opt/homebrew/bin/switchaudiosource", "-c")
	if err != nil {
		return "", err
	}
	switch source {
	case "DELL U3417W", "DELL U3824DW":
		return source, nil
	}
	return "", nil
}

func betterDisplayRequest(action, display, query string) (string, error) {
	// PathEscape: BetterDisplay wants %20 for spaces, not QueryEscape's "+"
	resp, err := httpClient.Get(betterDisplay + "/" + action + "?name=" + url.PathEscape(display) + "&" + query)
	if err != nil {
		return "", fmt.Errorf("BetterDisplay %s: %v", action, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("BetterDisplay %s body: %v", action, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("BetterDisplay %s: HTTP %d", action, resp.StatusCode)
	}
	return strings.TrimSuffix(string(body), "\n"), nil
}

func getMuteStatus() (bool, error) {
	output, err := getCommandOutput("/usr/bin/osascript", "-e", "output muted of (get volume settings)")
	if err != nil {
		return false, err
	}
	if output != "missing value" {
		return strconv.ParseBool(output)
	}
	// The output device has no macOS volume control (monitor audio): ask BetterDisplay.
	display, err := currentDisplay()
	if err != nil {
		return false, err
	}
	if display == "" {
		return false, errors.New("mute status unavailable: output is not a BetterDisplay monitor")
	}
	mute, err := betterDisplayRequest("get", display, "mute")
	if err != nil {
		return false, err
	}
	return mute == "on", nil
}

func getCurrentVolume() (int, error) {
	output, err := getCommandOutput("/usr/bin/osascript", "-e", "output volume of (get volume settings)")
	if err != nil {
		return 0, err
	}
	if i, err := strconv.Atoi(output); err == nil {
		return i, nil
	}
	display, err := currentDisplay()
	if err != nil {
		return 0, err
	}
	if display == "" {
		return 0, errors.New("volume unavailable: output is not a BetterDisplay monitor")
	}
	vol, err := betterDisplayRequest("get", display, "volume")
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(vol, 64)
	if err != nil {
		return 0, fmt.Errorf("BetterDisplay volume %q: %v", vol, err)
	}
	return int(f*100 + 0.5), nil
}

// from 0 to 100
func setVolume(i int) error {
	//Test first if we can control the volume if not use BetterDisplay
	test, err := getCommandOutput("/usr/bin/osascript", "-e", "output volume of (get volume settings)")
	if err != nil {
		return err
	}
	if test != "missing value" {
		_, err := getCommandOutput("/usr/bin/osascript", "-e", "set volume output volume "+strconv.Itoa(i))
		return err
	}
	display, err := currentDisplay()
	if err != nil {
		return err
	}
	if display == "" {
		return errors.New("cannot set volume: output is not a BetterDisplay monitor")
	}
	_, err = betterDisplayRequest("set", display, "volume="+fmt.Sprintf("%f", float64(i)/100))
	return err
}

// true - turn mute on
// false - turn mute off
func setMute(b bool) error {
	//Test first if we can control the mute if not use BetterDisplay
	test, err := getCommandOutput("/usr/bin/osascript", "-e", "output volume of (get volume settings)")
	if err != nil {
		return err
	}
	if test != "missing value" {
		_, err := getCommandOutput("/usr/bin/osascript", "-e", "set volume output muted "+strconv.FormatBool(b))
		return err
	}
	display, err := currentDisplay()
	if err != nil {
		return err
	}
	if display == "" {
		return errors.New("cannot set mute: output is not a BetterDisplay monitor")
	}
	state := "off"
	if b {
		state = "on"
	}
	_, err = betterDisplayRequest("set", display, "mute="+state)
	return err
}

func commandSleep() {
	runCommand("pmset", "sleepnow")
}

func commandDisplaySleep() {
	runCommand("pmset", "displaysleepnow")
}

func commandShutdown() {

	if os.Getuid() == 0 {
		// if the program is run by root user we are doing the most powerfull shutdown - that always shuts down the computer
		runCommand("shutdown", "-h", "now")
	} else {
		// if the program is run by ordinary user we are trying to shutdown, but it may fail if the other user is logged in
		runCommand("/usr/bin/osascript", "-e", "tell app \"System Events\" to shut down")
	}

}

func commandDisplayWake() {
	runCommand("/usr/bin/caffeinate", "-u", "-t", "1")
}

func commandKeepAwake() {
	// caffeinate keeps running in the background, so start it without waiting (and without the command timeout)
	if err := exec.Command("/usr/bin/caffeinate", "-d").Start(); err != nil {
		log.Println("Command failed: caffeinate: " + err.Error())
	}
}

func commandAllowSleep() {
	runShell("/bin/ps ax | /usr/bin/grep caffeinate | /usr/bin/grep -v grep | /usr/bin/awk '{print \"kill \"$1}'|sh")
}

func commandRunShortcut(shortcut string) {
	runCommand("shortcuts", "run", shortcut)
}

func commandScreensaver() {
	runCommand("open", "-a", "ScreenSaverEngine")
}

var messagePubHandler mqtt.MessageHandler = func(client mqtt.Client, msg mqtt.Message) {
	log.Printf("Received message: %s from topic: %s\n", msg.Payload(), msg.Topic())
	listen(client, msg)
}

// Runs on the first connect and on every reconnect: (re)announce availability, subscribe,
// re-send discovery and publish fresh state so HA isn't left with stale values.
var connectHandler mqtt.OnConnectHandler = func(client mqtt.Client) {
	log.Println("Connected to MQTT")

	publish(client, getTopicPrefix()+"/status/alive", true, "online")
	log.Println("Sending 'online' to topic: " + getTopicPrefix() + "/status/alive")
	sub(client, getTopicPrefix()+"/command/#")

	go func() {
		setDevice(client, cfg.DiscoveryPrefix)
		updateVolume(client)
		updateMute(client)
		updateBattery(client)
		updateCaffeinateStatus(client)
	}()
}

var connectLostHandler mqtt.ConnectionLostHandler = func(client mqtt.Client, err error) {
	log.Printf("Disconnected from MQTT: %v (reconnecting)", err)
}

var reconnectingHandler mqtt.ReconnectHandler = func(client mqtt.Client, opts *mqtt.ClientOptions) {
	log.Println("Reconnecting to MQTT")
}

func getMQTTClient(ip, port, user, password string) mqtt.Client {

	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("ssl://%s:%s", ip, port))
	opts.SetClientID("mac2mqtt-" + hostname)
	opts.SetUsername(user)
	opts.SetPassword(password)
	opts.SetKeepAlive(30 * time.Second)
	opts.SetConnectTimeout(10 * time.Second)
	// Keep trying instead of exiting when the broker or DNS isn't reachable yet (boot, wake, network change).
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(10 * time.Second)
	opts.SetAutoReconnect(true)
	opts.SetMaxReconnectInterval(time.Minute)
	// Handle each message in its own goroutine so a slow command can't block the client.
	opts.SetOrderMatters(false)
	opts.OnConnect = connectHandler
	opts.OnConnectionLost = connectLostHandler
	opts.OnReconnecting = reconnectingHandler
	opts.SetDefaultPublishHandler(messagePubHandler)

	opts.SetWill(getTopicPrefix()+"/status/alive", "offline", 0, true)

	client := mqtt.NewClient(opts)
	// With ConnectRetry the token only completes once connected; don't block startup on it.
	client.Connect()

	return client
}

func getTopicPrefix() string {
	return "mac2mqtt/" + hostname
}

func publish(client mqtt.Client, topic string, retained bool, payload interface{}) {
	if !client.IsConnectionOpen() {
		return
	}
	token := client.Publish(topic, 0, retained, payload)
	if token.WaitTimeout(10*time.Second) && token.Error() != nil {
		log.Printf("Publish to %s failed: %v", topic, token.Error())
	}
}

func sub(client mqtt.Client, topic string) {
	token := client.Subscribe(topic, 0, nil)
	if !token.WaitTimeout(10 * time.Second) {
		log.Printf("Subscribe to %s timed out", topic)
		return
	}
	if token.Error() != nil {
		log.Printf("Subscribe to %s failed: %v", topic, token.Error())
		return
	}
	log.Printf("Subscribed to topic: %s\n", topic)
}

func listen(client mqtt.Client, msg mqtt.Message) {
	if msg.Topic() == getTopicPrefix()+"/command/volume" {

		i, err := strconv.Atoi(string(msg.Payload()))
		if err == nil && i >= 0 && i <= 100 {

			if err := setVolume(i); err != nil {
				log.Println("Set volume failed: " + err.Error())
			}

			updateVolume(client)
			updateMute(client)

		} else {
			log.Println("Incorrect value")
		}

	}

	if msg.Topic() == getTopicPrefix()+"/command/mute" {

		b, err := strconv.ParseBool(string(msg.Payload()))
		if err == nil {
			if err := setMute(b); err != nil {
				log.Println("Set mute failed: " + err.Error())
			}

			updateVolume(client)
			updateMute(client)

		} else {
			log.Println("Incorrect value")
		}

	}

	if msg.Topic() == getTopicPrefix()+"/command/sleep" {

		if string(msg.Payload()) == "sleep" {
			commandSleep()
		}

	}

	if msg.Topic() == getTopicPrefix()+"/command/displaysleep" {

		if string(msg.Payload()) == "displaysleep" {
			commandDisplaySleep()
		}

	}

	if msg.Topic() == getTopicPrefix()+"/command/displaywake" {

		if string(msg.Payload()) == "displaywake" {
			commandDisplayWake()
		}

	}

	if msg.Topic() == getTopicPrefix()+"/command/shutdown" {

		if string(msg.Payload()) == "shutdown" {
			commandShutdown()
		}

	}

	if msg.Topic() == getTopicPrefix()+"/command/screensaver" {

		if string(msg.Payload()) == "screensaver" {
			commandScreensaver()
		}

	}

	if msg.Topic() == getTopicPrefix()+"/command/runshortcut" {
		commandRunShortcut(string(msg.Payload()))
	}

	if msg.Topic() == getTopicPrefix()+"/command/keepawake" {
		b, err := strconv.ParseBool(string(msg.Payload()))
		if err == nil {
			if b {
				commandKeepAwake()
			} else {
				commandAllowSleep()
			}
			updateCaffeinateStatus(client)
		} else {
			log.Println("Incorrect value")
		}
	}
}

// Last values read, so the log only shows changes and repeated errors aren't logged every minute.
var (
	stateMu   sync.Mutex
	lastState = map[string]string{}
)

func logChange(key, value string) {
	stateMu.Lock()
	defer stateMu.Unlock()
	if lastState[key] != value {
		log.Printf("%s: %s", key, value)
		lastState[key] = value
	}
}

func updateVolume(client mqtt.Client) {
	i, err := getCurrentVolume()
	if err != nil {
		logChange("Volume", "error: "+err.Error())
		return
	}
	logChange("Volume", strconv.Itoa(i))
	publish(client, getTopicPrefix()+"/status/volume", false, strconv.Itoa(i))
}

func updateMute(client mqtt.Client) {
	b, err := getMuteStatus()
	if err != nil {
		logChange("Mute", "error: "+err.Error())
		return
	}
	logChange("Mute", strconv.FormatBool(b))
	publish(client, getTopicPrefix()+"/status/mute", false, strconv.FormatBool(b))
}

func getBatteryChargePercent() string {

	output, err := getCommandOutput("/usr/bin/pmset", "-g", "batt")
	if err != nil {
		logChange("Battery", "error: "+err.Error())
		return ""
	}

	// $ /usr/bin/pmset -g batt
	// Now drawing from 'Battery Power'
	//  -InternalBattery-0 (id=4653155)        100%; discharging; 20:00 remaining present: true

	r := regexp.MustCompile(`(\d+)%`)
	res := r.FindStringSubmatch(output)
	if len(res) == 0 {
		return ""
	}

	return res[1]
}

func updateBattery(client mqtt.Client) {
	if pct := getBatteryChargePercent(); pct != "" {
		publish(client, getTopicPrefix()+"/status/battery", false, pct)
	}
}

func updateCaffeinateStatus(client mqtt.Client) {
	publish(client, getTopicPrefix()+"/status/caffeinate", false, strconv.FormatBool(getCaffeinateStatus()))
}

func setDevice(client mqtt.Client, DiscoveryPrefix string) {

	keepawake := map[string]interface{}{
		"p":               "switch",
		"name":            "Keep Awake",
		"unique_id":       hostname + "_keepwake",
		"entity_category": "config",
		"command_topic":   getTopicPrefix() + "/command/keepawake",
		"payload_on":      "true",
		"payload_off":     "false",
		"state_topic":     getTopicPrefix() + "/status/caffeinate",
		"icon":            "mdi:coffee",
	}

	displaywake := map[string]interface{}{
		"p":               "button",
		"name":            "Display Wake",
		"entity_category": "config",
		"unique_id":       hostname + "_displaywake",
		"command_topic":   getTopicPrefix() + "/command/displaywake",
		"payload_press":   "displaywake",
		"icon":            "mdi:monitor",
	}

	displaysleep := map[string]interface{}{
		"p":               "button",
		"name":            "Display Sleep",
		"entity_category": "config",
		"unique_id":       hostname + "_displaysleep",
		"command_topic":   getTopicPrefix() + "/command/displaysleep",
		"payload_press":   "displaysleep",
		"icon":            "mdi:monitor-off",
	}

	screensaver := map[string]interface{}{
		"p":               "button",
		"name":            "Screensaver",
		"unique_id":       hostname + "_screensaver",
		"entity_category": "config",
		"command_topic":   getTopicPrefix() + "/command/screensaver",
		"payload_press":   "screensaver",
		"icon":            "mdi:monitor-star",
	}

	sleep := map[string]interface{}{
		"p":               "button",
		"name":            "Sleep",
		"entity_category": "config",
		"unique_id":       hostname + "_sleep",
		"command_topic":   getTopicPrefix() + "/command/sleep",
		"payload_press":   "sleep",
		"icon":            "mdi:sleep",
	}

	shutdown := map[string]interface{}{
		"p":                  "button",
		"name":               "Shutdown",
		"entity_category":    "config",
		"unique_id":          hostname + "_shutdown",
		"command_topic":      getTopicPrefix() + "/command/shutdown",
		"payload_press":      "shutdown",
		"enabled_by_default": false,
		"icon":               "mdi:power",
	}
	mute := map[string]interface{}{
		"p":               "switch",
		"name":            "Mute",
		"entity_category": "config",
		"unique_id":       hostname + "_mute",
		"command_topic":   getTopicPrefix() + "/command/mute",
		"payload_on":      "true",
		"payload_off":     "false",
		"state_topic":     getTopicPrefix() + "/status/mute",
		"icon":            "mdi:volume-mute",
	}

	volume := map[string]interface{}{
		"p":               "number",
		"name":            "Volume",
		"unique_id":       hostname + "_volume",
		"entity_category": "config",
		"command_topic":   getTopicPrefix() + "/command/volume",
		"state_topic":     getTopicPrefix() + "/status/volume",
		"min_value":       0,
		"max_value":       100,
		"step":            1,
		"mode":            "slider",
		"icon":            "mdi:volume-high",
	}

	battery := map[string]interface{}{
		"p":                   "sensor",
		"name":                "Battery",
		"entity_category":     "config",
		"unique_id":           hostname + "_battery",
		"state_topic":         getTopicPrefix() + "/status/battery",
		"enabled_by_default":  false,
		"unit_of_measurement": "%",
		"device_class":        "battery",
	}

	components := map[string]interface{}{
		"sleep":        sleep,
		"shutdown":     shutdown,
		"volume":       volume,
		"mute":         mute,
		"displaywake":  displaywake,
		"displaysleep": displaysleep,
		"screensaver":  screensaver,
		"battery":      battery,
		"keepawake":    keepawake,
	}

	origin := map[string]interface{}{
		"name": "mac2mqtt",
	}

	device := map[string]interface{}{
		"ids":            serialNumber,
		"name":           hostname,
		"mf":             "Apple",
		"suggested_area": "Office",
		"mdl":            model,
	}

	object := map[string]interface{}{
		"dev":                device,
		"o":                  origin,
		"cmps":               components,
		"availability_topic": getTopicPrefix() + "/status/alive",
		"qos":                2,
	}
	objectJSON, _ := json.Marshal(object)

	publish(client, DiscoveryPrefix+"/device"+"/"+hostname+"/config", true, objectJSON)
}

// Read once at startup; they don't change while running.
var serialNumber, model string

func main() {

	log.Println("Started")
	cfg.getConfig()
	log.Println("Discovery Prefix: " + cfg.DiscoveryPrefix)

	hostname = cfg.Hostname
	serialNumber = getSerialnumber()
	model = getModel()

	mqttClient := getMQTTClient(cfg.Ip, cfg.Port, cfg.User, cfg.Password)

	volumeTicker := time.NewTicker(60 * time.Second)
	batteryTicker := time.NewTicker(60 * time.Second)
	awakeTicker := time.NewTicker(60 * time.Second)

	go func() {
		for {
			select {
			case <-volumeTicker.C:
				updateVolume(mqttClient)
				updateMute(mqttClient)

			case <-batteryTicker.C:
				updateBattery(mqttClient)
			case <-awakeTicker.C:
				updateCaffeinateStatus(mqttClient)
			}
		}
	}()

	// On launchctl stop/unload: mark offline right away instead of waiting for the broker to fire the will.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	<-sigs
	log.Println("Stopping")
	publish(mqttClient, getTopicPrefix()+"/status/alive", true, "offline")
	mqttClient.Disconnect(500)
}
