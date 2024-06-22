package main

import (
  "log"
  "time"
  "sync"
  "encoding/json"
  "github.com/gorilla/websocket"
)

type SessionSubIDs map[string]*[]interface{}
type SessionEventIDs map[string]map[string]struct{}
type SessionPendingEOSE map[string]int
type SessionRelays map[*websocket.Conn]struct{}

type Session struct {
  Owner *websocket.Conn
  Sub_IDs SessionSubIDs
  Event_IDs SessionEventIDs
  PendingEOSE SessionPendingEOSE
  Relays SessionRelays
  ready bool
  destroyed bool

  ownerWriteMu sync.Mutex
  eventMu sync.Mutex
  eoseMu sync.Mutex
  relaysMu sync.Mutex
  connWriteMu sync.Mutex
  subMu sync.Mutex
}

var dialer = websocket.Dialer{}

func (s *Session) Exist() bool {
  return s.Owner != nil
}

func (s *Session) NewConn(url string) {
  if s.destroyed {
    return
  }

  conn, resp, err := dialer.Dial(url, nil)

  if s.destroyed && conn != nil {
    conn.Close()
    return
  }

  if err != nil && !s.destroyed {
    s.Reconnect(conn, &url)
    return
  }

  if s.destroyed {
    if conn != nil {
      conn.Close()
    }
    return
  }

  if resp.StatusCode >= 500 {
    s.Reconnect(conn, &url)
    return
  } else if resp.StatusCode > 101 {
    log.Printf("Получил неожиданный код статуса от %s (%d). Больше не подключаюсь.\n", url, resp.StatusCode)
    return
  }

  s.relaysMu.Lock()
  s.Relays[conn] = struct{}{}
  s.relaysMu.Unlock()

  log.Printf("%s присоединился к нам.\n", url)

  s.OpenSubscriptions(conn)

  var stop bool = false

  for {
    var data []interface{}
    if err := conn.ReadJSON(&data); err != nil {
      return
    }

    if data == nil {
      return
    }

    switch data[0].(string) {
    case "EVENT":
      s.HandleUpstreamEVENT(data, &stop)
    case "EOSE":
      s.HandleUpstreamEOSE(data, &stop)
    }

    if stop {
      return
    }
  }

  conn.Close()

  if !stop {
    s.Reconnect(conn, &url)
  } else {
    log.Printf("%s: Отключение\n", url)
  }
}

func (s *Session) Reconnect(conn *websocket.Conn, url *string) {
  log.Printf("Произошла ошибка при подключении к %s. Повторная попытка через 5 секунд....\n", *url);

  s.relaysMu.Lock()
  delete(s.Relays, conn)
  s.relaysMu.Unlock()

  time.Sleep(5 * time.Second)
  if s.destroyed {
    return
  }
  go s.NewConn(*url)
}

func (s *Session) StartConnect() {
  for _, url := range config.Relays {
    if s.destroyed {
      return;
    }
    go s.NewConn(url);
  }
}

func (s *Session) Broadcast(data *[]interface{}) {
  JsonData, _ := json.Marshal(*data)

  s.relaysMu.Lock()
  defer s.relaysMu.Unlock()

  for relay := range s.Relays {
    s.connWriteMu.Lock()
    relay.WriteMessage(websocket.TextMessage, JsonData)
    s.connWriteMu.Unlock()
  }
}

func (s *Session) HasEvent(subid string, event_id string) bool {
  s.eventMu.Lock()
  defer s.eventMu.Unlock()
  events := s.Event_IDs[subid]
  if events == nil {
    return true
  }

  _, ok := events[event_id]

  if !ok {
    events[event_id] = struct{}{}
  }

  if len(events) > 500 {
    s.eoseMu.Lock()
    if _, ok := s.PendingEOSE[subid]; ok {
      delete(s.PendingEOSE, subid)
      s.WriteJSON(&[]interface{}{"EOSE", subid})
    }
    s.eoseMu.Unlock()
  }

  return ok
}

func (s *Session) HandleUpstreamEVENT(data []interface{}, stop *bool) {
  if len(data) < 3 {
    return
  }

  s.subMu.Lock()
  if _, ok := s.Sub_IDs[data[1].(string)]; !ok {
    s.subMu.Unlock()
    return
  }
  s.subMu.Unlock()

  if event := data[2].(map[string]interface{}); s.HasEvent(data[1].(string), event["id"].(string)) {
    return
  }

  if err := s.WriteJSON(&data); err != nil {
    *stop = true
    return
  }
}

func (s *Session) HandleUpstreamEOSE(data []interface{}, stop *bool) {
  if len(data) < 2 {
    return
  }

  s.eoseMu.Lock()
  defer s.eoseMu.Unlock()

  if _, ok := s.PendingEOSE[data[1].(string)]; !ok {
    return
  }

  s.PendingEOSE[data[1].(string)]++
  if s.PendingEOSE[data[1].(string)] >= len(config.Relays) {
    delete(s.PendingEOSE, data[1].(string))
    if err := s.WriteJSON(&data); err != nil {
      *stop = true
      return
    }
  }
}

/*
func (s *Session) CountEvents(subid string) int {
  return len(s.Event_IDs[subid])
}
*/

func (s *Session) WriteJSON(data *[]interface{}) error {
  JsonData, _ := json.Marshal(*data)

  s.ownerWriteMu.Lock()
  defer s.ownerWriteMu.Unlock()

  return s.Owner.WriteMessage(websocket.TextMessage, JsonData)
}

func (s *Session) OpenSubscriptions(conn *websocket.Conn) {
  s.subMu.Lock()
  defer s.subMu.Unlock()

  for id, filters := range s.Sub_IDs {
    ReqData := []interface{}{"REQ", id}
    ReqData = append(ReqData, *filters...)
    JsonData, _ := json.Marshal(ReqData)

    s.connWriteMu.Lock()
    conn.WriteMessage(websocket.TextMessage, JsonData)
    s.connWriteMu.Unlock()
  }
}

func (s *Session) Destroy(_ int, _ string) error {
  s.destroyed = true

  for relay := range s.Relays {
    relay.Close()
  }

  return nil
}

func (s *Session) REQ(data *[]interface{}) {
  if !s.ready {
    s.StartConnect()
    s.ready = true
  }

  subid := (*data)[1].(string)
  filters := (*data)[2:]

  s.CLOSE(data, false)

  s.eventMu.Lock()
  s.Event_IDs[subid] = make(map[string]struct{})
  s.eventMu.Unlock()

  s.eoseMu.Lock()
  s.PendingEOSE[subid] = 0
  s.eoseMu.Unlock()

  s.subMu.Lock()
  s.Sub_IDs[subid] = &filters;
  s.subMu.Unlock()

  s.Broadcast(data)
}

func (s *Session) CLOSE(data *[]interface{}, sendClosed bool) {
  subid := (*data)[1].(string)

  s.eventMu.Lock()
  delete(s.Event_IDs, subid)
  s.eventMu.Unlock()

  s.subMu.Lock()
  delete(s.Sub_IDs, subid)
  s.subMu.Unlock()

  s.eoseMu.Lock()
  delete(s.PendingEOSE, subid)
  s.eoseMu.Unlock()

  if sendClosed {
    s.WriteJSON(&[]interface{}{"CLOSED", subid, ""})
  }

  s.Broadcast(data)
}

func (s *Session) EVENT(data *[]interface{}) {
  if !s.ready {
    s.StartConnect()
    s.ready = true
  }

  event := (*data)[1].(map[string]interface{})
  id, ok := event["id"]
  if !ok {
    s.WriteJSON(&[]interface{}{"NOTICE", "Неверный объект."})
    return
  }

  s.WriteJSON(&[]interface{}{"OK", id, true, ""})
  s.Broadcast(data)
}
