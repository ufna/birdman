package agentlink

import (
	"testing"

	agentlinkv1 "github.com/ufna/birdman/proto/agentlink/v1"
)

// Каждый командный тип oneof обязан присутствовать в ОБОИХ switch'ах
// (stampCmdID и commandID): пропуск даёт cmd_id="" на wire — агент
// защитно скипает такое сообщение как не-команду (recvLoop: cmdID=="" →
// continue), ack не приходит, очередь ноды виснет навечно. Ровно так
// W3-RemoveImage завис на Фазе D 14.07.2026: обе стороны были
// реализованы, а hub-switch'и — нет; fake-sender юнитов это не ловит.
// Таблица перечисляет ВСЕ типы — новая команда без строки здесь падает
// в ревью, а без case'ов в hub — на этом тесте.
func TestCmdIDCoversEveryCommandType(t *testing.T) {
	msgs := map[string]*agentlinkv1.MasterMsg{
		"start":          {Msg: &agentlinkv1.MasterMsg_Start{Start: &agentlinkv1.StartServer{}}},
		"stop":           {Msg: &agentlinkv1.MasterMsg_Stop{Stop: &agentlinkv1.StopServer{}}},
		"prepull":        {Msg: &agentlinkv1.MasterMsg_Prepull{Prepull: &agentlinkv1.PrePull{}}},
		"drain":          {Msg: &agentlinkv1.MasterMsg_Drain{Drain: &agentlinkv1.Drain{}}},
		"undrain":        {Msg: &agentlinkv1.MasterMsg_Undrain{Undrain: &agentlinkv1.Undrain{}}},
		"upgrade":        {Msg: &agentlinkv1.MasterMsg_Upgrade{Upgrade: &agentlinkv1.UpgradeAgent{}}},
		"tail":           {Msg: &agentlinkv1.MasterMsg_Tail{Tail: &agentlinkv1.TailLogs{}}},
		"ack":            {Msg: &agentlinkv1.MasterMsg_Ack{Ack: &agentlinkv1.Ack{}}},
		"allocate":       {Msg: &agentlinkv1.MasterMsg_Allocate{Allocate: &agentlinkv1.AllocateServer{}}},
		"drain_server":   {Msg: &agentlinkv1.MasterMsg_DrainServer{DrainServer: &agentlinkv1.DrainServer{}}},
		"set_registries": {Msg: &agentlinkv1.MasterMsg_SetRegistries{SetRegistries: &agentlinkv1.SetRegistries{}}},
		"remove_image":   {Msg: &agentlinkv1.MasterMsg_RemoveImage{RemoveImage: &agentlinkv1.RemoveImage{}}},
	}
	for name, m := range msgs {
		stampCmdID(m, "cmd-"+name)
		if got := commandID(m); got != "cmd-"+name {
			t.Errorf("%s: stamp/read cmd_id broken: got %q (пропущен case в hub-switch'ах?)", name, got)
		}
	}
}
