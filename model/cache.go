package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"one-api/common"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	TokenCacheSeconds         = common.SyncFrequency
	UserId2GroupCacheSeconds  = common.SyncFrequency
	UserId2QuotaCacheSeconds  = common.SyncFrequency
	UserId2StatusCacheSeconds = common.SyncFrequency
)

func CacheGetTokenByKey(key string) (*Token, error) {
	keyCol := "`key`"
	if common.UsingPostgreSQL {
		keyCol = `"key"`
	}
	var token Token
	if !common.RedisEnabled {
		err := DB.Where(keyCol+" = ?", key).First(&token).Error
		return &token, err
	}
	tokenObjectString, err := common.RedisGet(fmt.Sprintf("token:%s", key))
	if err != nil {
		err := DB.Where(keyCol+" = ?", key).First(&token).Error
		if err != nil {
			return nil, err
		}
		jsonBytes, err := json.Marshal(token)
		if err != nil {
			return nil, err
		}
		err = common.RedisSet(fmt.Sprintf("token:%s", key), string(jsonBytes), time.Duration(TokenCacheSeconds)*time.Second)
		if err != nil {
			common.SysError("Redis set token error: " + err.Error())
		}
		return &token, nil
	}
	err = json.Unmarshal([]byte(tokenObjectString), &token)
	return &token, err
}

func CacheGetUserGroup(id int) (group string, err error) {
	if !common.RedisEnabled {
		return GetUserGroup(id)
	}
	group, err = common.RedisGet(fmt.Sprintf("user_group:%d", id))
	if err != nil {
		group, err = GetUserGroup(id)
		if err != nil {
			return "", err
		}
		err = common.RedisSet(fmt.Sprintf("user_group:%d", id), group, time.Duration(UserId2GroupCacheSeconds)*time.Second)
		if err != nil {
			common.SysError("Redis set user group error: " + err.Error())
		}
	}
	return group, err
}

func CacheGetUserQuota(id int) (quota int, err error) {
	if !common.RedisEnabled {
		return GetUserQuota(id)
	}
	quotaString, err := common.RedisGet(fmt.Sprintf("user_quota:%d", id))
	if err != nil {
		quota, err = GetUserQuota(id)
		if err != nil {
			return 0, err
		}
		err = common.RedisSet(fmt.Sprintf("user_quota:%d", id), fmt.Sprintf("%d", quota), time.Duration(UserId2QuotaCacheSeconds)*time.Second)
		if err != nil {
			common.SysError("Redis set user quota error: " + err.Error())
		}
		return quota, err
	}
	quota, err = strconv.Atoi(quotaString)
	return quota, err
}

func CacheUpdateUserQuota(id int) error {
	if !common.RedisEnabled {
		return nil
	}
	quota, err := GetUserQuota(id)
	if err != nil {
		return err
	}
	err = common.RedisSet(fmt.Sprintf("user_quota:%d", id), fmt.Sprintf("%d", quota), time.Duration(UserId2QuotaCacheSeconds)*time.Second)
	return err
}

func CacheDecreaseUserQuota(id int, quota int) error {
	if !common.RedisEnabled {
		return nil
	}
	err := common.RedisDecrease(fmt.Sprintf("user_quota:%d", id), int64(quota))
	return err
}

func CacheIsUserEnabled(userId int) (bool, error) {
	if !common.RedisEnabled {
		return IsUserEnabled(userId)
	}
	enabled, err := common.RedisGet(fmt.Sprintf("user_enabled:%d", userId))
	if err == nil {
		return enabled == "1", nil
	}

	userEnabled, err := IsUserEnabled(userId)
	if err != nil {
		return false, err
	}
	enabled = "0"
	if userEnabled {
		enabled = "1"
	}
	err = common.RedisSet(fmt.Sprintf("user_enabled:%d", userId), enabled, time.Duration(UserId2StatusCacheSeconds)*time.Second)
	if err != nil {
		common.SysError("Redis set user enabled error: " + err.Error())
	}
	return userEnabled, err
}

var group2model2channels map[string]map[string][]*Channel
var channelsIDM map[int]*Channel
var channelSyncLock sync.RWMutex

func InitChannelCache() {
	newChannelId2channel := make(map[int]*Channel)
	var channels []*Channel
	DB.Where("status = ?", common.ChannelStatusEnabled).Find(&channels)
	for _, channel := range channels {
		newChannelId2channel[channel.Id] = channel
	}
	var abilities []*Ability
	DB.Find(&abilities)
	groups := make(map[string]bool)
	for _, ability := range abilities {
		groups[ability.Group] = true
	}
	newGroup2model2channels := make(map[string]map[string][]*Channel)
	newChannelsIDM := make(map[int]*Channel)
	for group := range groups {
		newGroup2model2channels[group] = make(map[string][]*Channel)
	}
	for _, channel := range channels {
		newChannelsIDM[channel.Id] = channel
		groups := strings.Split(channel.Group, ",")
		for _, group := range groups {
			models := strings.Split(channel.Models, ",")
			for _, model := range models {
				if _, ok := newGroup2model2channels[group][model]; !ok {
					newGroup2model2channels[group][model] = make([]*Channel, 0)
				}
				newGroup2model2channels[group][model] = append(newGroup2model2channels[group][model], channel)
			}
		}
	}

	// sort by priority
	for group, model2channels := range newGroup2model2channels {
		for model, channels := range model2channels {
			sort.Slice(channels, func(i, j int) bool {
				return channels[i].GetPriority() > channels[j].GetPriority()
			})
			newGroup2model2channels[group][model] = channels
		}
	}

	channelSyncLock.Lock()
	group2model2channels = newGroup2model2channels
	channelsIDM = newChannelsIDM
	channelSyncLock.Unlock()
	common.SysLog("channels synced from database")
}

func SyncChannelCache(frequency int) {
	for {
		time.Sleep(time.Duration(frequency) * time.Second)
		common.SysLog("syncing channels from database")
		InitChannelCache()
	}
}

func CacheGetRandomSatisfiedChannel(group string, model string) (*Channel, error) {
	if strings.HasPrefix(model, "gpt-4-gizmo") {
		model = "gpt-4-gizmo-*"
	}

	// if memory cache is disabled, get channel directly from database
	if !common.MemoryCacheEnabled {
		return GetRandomSatisfiedChannel(group, model)
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()
	channels := group2model2channels[group][model]
	if len(channels) == 0 {
		return nil, errors.New("channel not found")
	}
	endIdx := len(channels)
	// choose by priority
	firstChannel := channels[0]
	if firstChannel.GetPriority() > 0 {
		for i := range channels {
			if channels[i].GetPriority() != firstChannel.GetPriority() {
				endIdx = i
				break
			}
		}
	}
	// Calculate the total weight of all channels up to endIdx
	totalWeight := 0
	for _, channel := range channels[:endIdx] {
		totalWeight += channel.GetWeight()
	}

	var selectedChannel *Channel
	for {
		if totalWeight == 0 {
			// 如果所有渠道的权重都为0，则随机选择一个
			selectedChannel = channels[rand.Intn(endIdx)]
		} else {
			// 在权重范围内生成一个随机值
			randomWeight := rand.Intn(totalWeight)

			// 根据权重选择渠道
			for _, channel := range channels[:endIdx] {
				randomWeight -= channel.GetWeight()
				if randomWeight <= 0 {
					selectedChannel = channel
					break
				}
			}
			// 如果没有基于权重找到渠道，则选择最后一个渠道
			if selectedChannel == nil {
				selectedChannel = channels[endIdx-1]
			}
		}

		// 检查选定的渠道是否开启了频率限制并且是否达到了频率限制
		if selectedChannel.RateLimited != nil && *selectedChannel.RateLimited {
			_, ok := checkRateLimit(selectedChannel.Id)
			if !ok { // 如果已经达到频率限制
				// 如果当前渠道的频率限制被打开且达到限制，则从列表中移除并重新进行选择
				channels = append(channels[:endIdx], channels[endIdx+1:]...)
				endIdx--
				if endIdx == 0 {
					return nil, errors.New("no channels available within rate limits")
				}
				// 回到循环的开始，重新选择渠道
				continue
			}

			// 更新频率限制状态，因为通过了检查并且能够使用该渠道
			updateRateLimitStatus(selectedChannel.Id)
		}

		break // 找到合适的渠道，跳出循环
	}

	// 返回最终选定的渠道
	return selectedChannel, nil
}

func CacheGetChannel(id int) (*Channel, error) {
	if !common.MemoryCacheEnabled {
		return GetChannelById(id, true)
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	c, ok := channelsIDM[id]
	if !ok {
		return nil, errors.New(fmt.Sprintf("当前渠道# %d，已不存在", id))
	}
	return c, nil
}
