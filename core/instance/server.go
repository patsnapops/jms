package instance

import (
	"strings"
	"time"

	"github.com/alibabacloud-go/tea/tea"
	"github.com/patrickmn/go-cache"
	"github.com/xops-infra/multi-cloud-sdk/pkg/model"
	"github.com/xops-infra/noop/log"

	"github.com/xops-infra/jms/app"
	"github.com/xops-infra/jms/config"
	"github.com/xops-infra/jms/core/db"
)

// 支持数据库和配置文件两种方式获取 KEY以及 Profile.
func LoadServer(conf *config.Config) {
	defer func() {
		if err := recover(); err != nil {
			log.Errorf("LoadServer panic: %v", err)
		}
	}()

	var mcsServers []model.Instance
	startTime := time.Now()
	for _, profile := range conf.Profiles {
		log.Debugf(tea.Prettify(profile))
		log.Debugf("profile: %s is enabled: %t", *profile.Name, profile.Enabled)
		if !profile.Enabled {
			continue
		}
		for _, region := range profile.Regions {
			log.Infof("get instances profile: %s region: %s", *profile.Name, region)
			input := model.InstanceFilter{}
			for {
				resps, err := app.App.McsServer.DescribeInstances(*profile.Name, region, input)
				if err != nil {
					log.Errorf("%s %s DescribeInstances error: %v", *profile.Name, region, err)
					break
				}
				mcsServers = append(mcsServers, resps.Instances...)
				if resps.NextMarker == nil {
					break
				}
				input.NextMarker = resps.NextMarker
			}
		}
	}
	instanceAll := fmtServer(mcsServers, conf.Keys.ToMap())
	app.App.Cache.Set("servers", instanceAll, cache.NoExpiration)
	log.Infof("%s len: %d", time.Since(startTime), len(instanceAll))
}

func fmtServer(instances []model.Instance, keys map[string]db.AddKeyRequest) config.Servers {
	var instanceAll []config.Server
	for _, instance := range instances {
		if instance.Status != model.InstanceStatusRunning {
			continue
		}
		var keyName []*string
		for _, key := range instance.KeyIDs {
			if key == nil {
				break
			}
			// 解决 key大写不识别问题
			key = tea.String(strings.ToLower(*key))
			if _, ok := keys[*key]; ok {
				keyName = append(keyName, keys[*key].IdentityFile)
			} else {
				log.Warnf("instance:%s key:%s not found in config", *instance.Name, *key)
				continue
			}
		}
		if keyName == nil {
			// 只有被 jms配置纪录的 key才会被接管，否则会出现无法登录情况。
			continue
		}
		// log.Infof("instance:%s key: %s ips:%s\n", *instance.Name, *keyName, *instance.PrivateIP[0])
		if len(instance.PrivateIP) < 1 {
			log.Errorf("instance: %s private ip is empty", *instance.Name)
			continue
		}
		sshUser := fmtSuperUser(instance)
		newInstance := config.Server{
			ID:       *instance.InstanceID,
			Name:     tea.StringValue(instance.Name),
			Host:     *instance.PrivateIP[0],
			Port:     22,
			Profile:  instance.Profile,
			Region:   tea.StringValue(instance.Region),
			Status:   instance.Status,
			KeyPairs: keyName,
			SSHUsers: sshUser,
			Tags:     *instance.Tags,
		}
		instanceAll = append(instanceAll, newInstance)
	}
	return instanceAll
}

func GetServers() config.Servers {
	servers, found := app.App.Cache.Get("servers")
	if !found {
		return nil
	}
	return servers.(config.Servers)
}

// 通过机器的密钥对 Key Name 获取对应的密钥Pem的路径
func getKeyPair(keyNames []*string) []db.AddKeyRequest {
	keysAll := make([]db.AddKeyRequest, 0)
	configKeys := app.App.Config.Keys.ToMap()
	for _, keyName := range keyNames {
		if keyName == nil {
			continue
		}
		lowKey := strings.ToLower(*keyName)
		if _, ok := configKeys[lowKey]; ok {
			keysAll = append(keysAll, configKeys[lowKey])
		}
	}
	return keysAll
}

// fmtSuperUser 支持多用户选择
func fmtSuperUser(instance model.Instance) []config.SSHUser {
	keys := getKeyPair(instance.KeyIDs)
	var sshUser []config.SSHUser
	for _, key := range keys {
		u := config.SSHUser{}
		if key.IdentityFile != nil {
			u.KeyName = tea.StringValue(key.IdentityFile)
		}
		if key.PemBase64 != nil {
			u.Base64Pem = tea.StringValue(key.PemBase64)
		}

		if strings.Contains(*instance.Platform, "Ubuntu") {
			u.SSHUsername = "ubuntu"
		} else if *instance.Platform == "Linux/UNIX" {
			u.SSHUsername = "ec2-user"
		} else {
			u.SSHUsername = "root"
		}
		sshUser = append(sshUser, u)
	}
	return sshUser
}
