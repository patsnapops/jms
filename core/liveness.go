package core

import (
	"context"
	"fmt"
	"time"

	dt "github.com/xops-infra/go-dingtalk-sdk-wrapper"
	"github.com/xops-infra/noop/log"

	"github.com/xops-infra/jms/app"
	"github.com/xops-infra/jms/core/sshd"
)

// dingtalkToken 为钉钉机器人的token
func ServerLiveness(dingtalkToken string) {
	timeStart := time.Now()
	servers, err := app.App.DBIo.LoadServer()
	if err != nil {
		log.Errorf("server liveness check error: %s", err)
		return
	}
	serversMap := servers.ToMap()
	// 获取实时 keys
	keys, err := app.App.DBIo.InternalLoadKey()
	if err != nil {
		log.Errorf("server liveness check error: %s", err)
		return
	}

	// 以配置文件为准，检查配置文件中的机器是否存在
	for _, checkIp := range app.App.Config.WithSSHCheck.IPS {
		if _, ok := serversMap[checkIp]; !ok {
			log.Errorf("server liveness check error: %s not found", checkIp)
			SendMessage(dingtalkToken, fmt.Sprintf("（紧急）机器 %s 不存在，请检查机器是否存在，若已经下线请及时更新配置", checkIp))
			continue
		}
		server := serversMap[checkIp]
		log.Infof("server liveness check: %s", server.Host)
		sshUsers, err := app.App.Sshd.SshdIO.GetSSHUsersByHost(server.Host, serversMap, keys)
		if err != nil {
			log.Errorf("server liveness check error: %s", err)
			continue
		}

		for _, sshUser := range sshUsers {
			proxyClient, client, err := sshd.NewSSHClient("system_liveness_check", server, sshUser)
			if err != nil {
				_, found := app.App.Config.WithSSHCheck.LivenessCache.Get(server.Host)
				if found {
					return
				}
				app.App.Config.WithSSHCheck.LivenessCache.Add(server.Host, 1, 0)
				SendMessage(dingtalkToken, fmt.Sprintf("（紧急）机器ssh连接失败，请检查机器是否失联！\n机器名称：%s\n机器IP：%s\n登录用户：%s\n告警时间：%s\n错误信息：%s", server.Name, server.Host,
					sshUser.UserName, time.Now().Format(time.RFC3339), err))
				continue
			}
			if proxyClient != nil {
				defer proxyClient.Close()
			}
			defer client.Close()

			if false {
				// TODO: 这里可以实现对目标机器的自定义命令检查
				sess, _ := client.NewSession()
				defer sess.Close()
				// 执行命令
				info, err := sess.Output(`netstat -an | grep ESTABLISHED | grep ":22" | wc -l`)
				if err != nil {
					log.Errorf("server liveness check error: %s", err)
				}
				log.Infof("server %s SSH ESTABLISHED nu: %s ", server.Host, string(info))
			}

			_, found := app.App.Config.WithSSHCheck.LivenessCache.Get(server.Host)
			if found {
				SendMessage(dingtalkToken, fmt.Sprintf("机器ssh连接已经恢复！\n机器名称：%s\n机器IP：%s\n告警时间：%s\n登录用户：%s", server.Name, server.Host, time.Now().Format(time.RFC3339), sshUser.UserName))
				app.App.Config.WithSSHCheck.LivenessCache.Delete(server.Host)
			}
			break // 只检查一个
		}

	}
	log.Infof("server liveness check done cost: %s", time.Since(timeStart))
}

// 发送到群里
func SendMessage(token, msg string) {
	log.Infof("send dingtalk msg: %s", msg)
	err := app.App.Scheduler.RobotClient.SendMessage(context.Background(), &dt.SendMessageRequest{
		AccessToken: token,
		MessageContent: dt.MessageContent{
			MsgType: "text",
			Text: dt.TextBody{
				Content: msg,
			},
		},
	})
	if err != nil {
		log.Errorf("send dingtalk msg error: %s", err)
	}
	log.Infof("send dingtalk msg: %s", msg)
}
