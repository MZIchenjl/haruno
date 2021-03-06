package coolq

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/haruno-bot/haruno/clients"
	"github.com/haruno-bot/haruno/logger"
)

const timeForWait = 30

const noFilterKey = "__NEVER_SET_UNUSED_KEY__"

// Filter 过滤函数
type Filter func(*CQEvent) bool

// Handler 处理函数
type Handler func(*CQEvent)

type pluginEntry struct {
	keys     []string
	fitlers  map[string]Filter
	handlers map[string]Handler
}

// cqclient 酷q机器人连接客户端
// 为了安全起见，暂时不允许在包外额外创建
type cqclient struct {
	mu            sync.Mutex
	token         string
	apiConn       *clients.WSClient
	eventConn     *clients.WSClient
	httpConn      *clients.HTTPClient
	apiURL        string
	pluginEntries map[string]pluginEntry
	echoqueue     map[int64]bool
}

func handleConnect(conn *clients.WSClient) {
	if conn.IsConnected() {
		logger.Successf("%s has been connected successfully!", conn.Name)
	}
}

// RegisterAllPlugins 注册所有的插件
func (c *cqclient) RegisterAllPlugins() {
	// 1. 先全部执行加载函数
	loaded := make([]PluginInterface, 0)
	for _, plug := range entries {
		err := plug.Load()
		if err != nil {
			logger.Errorf("Plugin %s can't be loaded, reason:\n %v", plug.Name(), err)
			continue
		}
		loaded = append(loaded, plug)
	}
	// 2. 注册所有的handler和filter
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, plug := range loaded {
		pluginName := plug.Name()
		pluginFilters := plug.Filters()
		pluginHandlers := plug.Handlers()
		hasFilter := make(map[string]bool)
		entry := pluginEntry{
			keys:     make([]string, 0),
			fitlers:  make(map[string]Filter),
			handlers: make(map[string]Handler),
		}
		noFilterHanlers := make([]Handler, 0)
		// 对应filter的key寻找相应的handler， 没有的话则给出警告
		for key, filter := range pluginFilters {
			handler := pluginHandlers[key]
			if handler == nil {
				logger.Logger.Warnf("插件 %s 中存在没有使用的key: %s\n", pluginName, key)
				continue
			}
			hasFilter[key] = true
			entry.keys = append(entry.keys, key)
			entry.fitlers[key] = filter
			entry.handlers[key] = handler
		}
		for key, handler := range pluginHandlers {
			if !hasFilter[key] {
				noFilterHanlers = append(noFilterHanlers, handler)
			}
		}
		// 最后注册无key的handler
		entry.handlers[noFilterKey] = func(event *CQEvent) {
			for _, hanldeFunc := range noFilterHanlers {
				hanldeFunc(event)
			}
		}
		c.pluginEntries[pluginName] = entry
	}
	// 3. 触发所有插件的onload事件
	for _, plug := range loaded {
		go plug.Loaded()
	}
}

func (c *cqclient) deqEcho(echo int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.echoqueue, echo)
}

// Initialize 初始化客户端
// token 酷q机器人的access token
func (c *cqclient) Initialize(token string) {
	c.token = token
	c.httpConn = clients.NewHTTPClient()
	c.httpConn.Header.Set("Authorization", fmt.Sprintf("Token %s", c.token))

	c.apiConn.Name = "coolq api conn"
	c.eventConn.Name = "coolq event conn"
	// 注册连接事件回调
	c.apiConn.OnConnect = handleConnect
	c.eventConn.OnConnect = handleConnect
	// 注册错误事件回调
	c.apiConn.OnError = func(err error) {
		logger.Field(c.apiConn.Name).Error(err)
	}
	c.eventConn.OnError = func(err error) {
		logger.Field(c.eventConn.Name).Error(err)
	}
	// 注册消息事件回调
	c.apiConn.OnMessage = func(raw []byte) {
		msg := new(CQResponse)
		err := json.Unmarshal(raw, msg)
		if err != nil {
			logger.Field(c.apiConn.Name).Errorf("on message error %v", err)
			return
		}
		// echo队列 - 确定发送消息是否超时
		echo := msg.Echo
		if c.echoqueue[echo] {
			c.deqEcho(echo)
		}
	}
	// 注册上报事件回调
	c.eventConn.OnMessage = func(raw []byte) {
		event := new(CQEvent)
		err := json.Unmarshal(raw, event)
		if err != nil {
			logger.Field(c.eventConn.Name).Errorf("on message error %v", err)
			return
		}
		for name, entry := range c.pluginEntries {
			// 先异步处理没有key的回调
			go entry.handlers[noFilterKey](event)
			// 一次异步执行所有的 filter 和 handler 对
			for _, key := range entry.keys {
				go func(key string, name string) {
					if c.pluginEntries[name].fitlers[key](event) {
						c.pluginEntries[name].handlers[key](event)
					}
				}(key, name)
			}
		}
	}

	// 定时清理echo队列 (30s)
	go func() {
		ticker := time.NewTicker(timeForWait * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				now := time.Now().Unix()
				for echo, state := range c.echoqueue {
					// 对于超过30s未响应的给出提示
					if state && now-echo > timeForWait {
						logger.Errorf("(echo) id = %d response time out (30s)", echo)
						c.deqEcho(echo)
					}
				}
			}
		}
	}()
}

// Connect 连接远程酷q api服务
// wsURL 形如 ws://127.0.0.1:8080, wss://127.0.0.1:8080之类的url 用于建立ws连接
// httpURL 形如 http://127.0.0.1:8080之类的url 用户建立http”连接“
func (c *cqclient) Connect(wsURL, httpURL string) {
	headers := make(http.Header)
	headers.Add("Authorization", fmt.Sprintf("Token %s", c.token))
	// 连接api服务和事件服务
	c.apiConn.Dial(fmt.Sprintf("%s/api", wsURL), headers)
	c.eventConn.Dial(fmt.Sprintf("%s/event", wsURL), headers)
	c.apiURL = httpURL
}

// IsAPIOk api服务是否可用
func (c *cqclient) IsAPIOk() bool {
	return c.apiConn.IsConnected()
}

// IsEventOk event服务是否可用
func (c *cqclient) IsEventOk() bool {
	return c.eventConn.IsConnected()
}

// APISendJSON 发送api json格式的数据
func (c *cqclient) APISendJSON(data interface{}) {
	if !c.IsAPIOk() {
		return
	}
	msg, _ := json.Marshal(data)
	c.apiConn.Send(websocket.TextMessage, msg)
}

// SendGroupMsg 发送群消息
// websocket 接口
func (c *cqclient) SendGroupMsg(groupID int64, message string) {
	payload := &CQWSMessage{
		Action: ActionSendGroupMsg,
		Params: CQTypeSendGroupMsg{
			GroupID: groupID,
			Message: message,
		},
		Echo: time.Now().Unix(),
	}
	c.APISendJSON(payload)
}

// SendPrivateMsg 发送私聊消息
// websocket 接口
func (c *cqclient) SendPrivateMsg(userID int64, message string) {
	payload := &CQWSMessage{
		Action: ActionSendPrivateMsg,
		Params: CQTypeSendPrivateMsg{
			UserID:  userID,
			Message: message,
		},
		Echo: time.Now().Unix(),
	}
	c.APISendJSON(payload)
}

// SetGroupKick 群组踢人
// reject 是否拒绝加群申请
// websocket 接口
func (c *cqclient) SetGroupKick(groupID, userID int64, reject bool) {
	payload := &CQWSMessage{
		Action: ActionSetGroupKick,
		Params: CQTypeSetGroupKick{
			GroupID:          groupID,
			UserID:           userID,
			RejectAddRequest: reject,
		},
		Echo: time.Now().Unix(),
	}
	c.APISendJSON(payload)
}

// SetGroupBan 群组单人禁言
// duration 禁言时长，单位秒，0 表示取消禁言
// websocket 接口
func (c *cqclient) SetGroupBan(groupID, userID int64, duration int64) {
	payload := &CQWSMessage{
		Action: ActionSetGroupBan,
		Params: CQTypeSetGroupBan{
			GroupID:  groupID,
			UserID:   userID,
			Duration: duration,
		},
		Echo: time.Now().Unix(),
	}
	c.APISendJSON(payload)
}

// SetGroupWholeBan 群组全员禁言
// enable 是否禁言
// websocket 接口
func (c *cqclient) SetGroupWholeBan(groupID int64, enable bool) {
	payload := &CQWSMessage{
		Action: ActionSetGroupWholeBan,
		Params: CQTypeSetGroupWholeBan{
			GroupID: groupID,
			Enable:  enable,
		},
		Echo: time.Now().Unix(),
	}
	c.APISendJSON(payload)
}

func warnHTTPApiURLNotSet() {
	logger.Logger.Warnln("Try to request a http api url, but no http api url was set.")
}

func (c *cqclient) getAPIURL(api string) string {
	return fmt.Sprintf("%s/%s", c.apiURL, api)
}

// GetStatus 获取插件运行状态
// http 接口
func (c *cqclient) GetStatus() *CQTypeGetStatus {
	if c.apiURL == "" {
		warnHTTPApiURLNotSet()
		return nil
	}
	url := c.getAPIURL(ActionGetStatus)
	res, err := c.httpConn.Get(url)
	if err != nil {
		logger.Errorf("cqclient http method getStatus error: %v", err)
		return nil
	}
	defer res.Body.Close()
	response := new(CQResponse)
	if err := json.NewDecoder(res.Body).Decode(response); err != nil {
		logger.Errorf("cqclient http method getStatus error: %v", err)
		return nil
	}
	if response.RetCode != 0 {
		return nil
	}
	data := response.Data.(map[string]interface{})
	status := new(CQTypeGetStatus)
	status.AppInitialized = data["app_initialized"].(bool)
	status.AppEnabled = data["app_enabled"].(bool)
	status.PluginsGood = data["plugins_good"].(bool)
	status.AppGood = data["app_good"].(bool)
	status.Online = data["online"].(bool)
	status.Good = data["good"].(bool)
	return status
}

// Client 唯一的酷q机器人实体
var Client = &cqclient{
	apiConn:       new(clients.WSClient),
	eventConn:     new(clients.WSClient),
	pluginEntries: make(map[string]pluginEntry),
}
