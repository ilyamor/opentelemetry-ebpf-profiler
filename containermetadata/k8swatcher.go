package containermetadata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"

	log "github.com/sirupsen/logrus"
)

type RedisConfig struct {
	Host    string
	Port    int
	Channel string
}

type K8sWatcherConfig struct {
	URL           string
	Timeout       time.Duration
	RetryAttempts int
}

type K8sEnricherConfig struct {
	Redis      RedisConfig
	K8sWatcher K8sWatcherConfig
}

type K8sStateResponse struct {
	Entities []K8sEntity `json:"entities"`
}

type K8sEvent struct {
	Type   string    `json:"type"`
	Entity K8sEntity `json:"entity,omitempty"`
}

type K8sEntity struct {
	Pod K8sPod `json:"pod,omitempty"`
}

type K8sPod struct {
	Name       string         `json:"name"`
	Namespace  string         `json:"namespace"`
	NodeName   string         `json:"node_name"`
	Containers []K8sContainer `json:"containers"`
	Ips        []string       `json:"ips"`
	Owner      K8sOwner       `json:"owner,omitempty"`
}

type K8sService struct {
	Name           string   `json:"name"`
	Namespace      string   `json:"namespace"`
	ClusterIps     []string `json:"cluster_ips"`
	SharedPodOwner K8sOwner `json:"shared_pod_owner,omitempty"`
}

type K8sNode struct {
	Name string   `json:"name"`
	Ips  []string `json:"ips"`
}

type K8sOwner struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type IpOwnerType string

const (
	Pod     IpOwnerType = "Pod"
	Service IpOwnerType = "Service"
	Node    IpOwnerType = "Node"
)

type IpOwner struct {
	IpOwnerType IpOwnerType `json:"ip_owner_type"`
	Owner       K8sOwner    `json:"owner,omitempty"`
}

type K8sContainer struct {
	Id   string `json:"id"`
	Name string `json:"name"`
}

type K8sEnricher struct {
	redisClient *redis.Client
	k8sClient   *http.Client
	config      K8sEnricherConfig
	cache       *sync.Map
}

type K8sRedisEvent struct {
	Modified *K8sEntity `json:"Modified,omitempty"`
	Deleted  *K8sEntity `json:"Deleted,omitempty"`
	Refresh  bool       `json:"Refresh,omitempty"`
}

func NewK8sEnricher(config K8sEnricherConfig) *K8sEnricher {
	redisClient := redis.NewClient(&redis.Options{
		Addr: fmt.Sprintf("%s:%d", config.Redis.Host, config.Redis.Port),
	})

	k8sClient := &http.Client{
		Timeout: config.K8sWatcher.Timeout,
	}
	var cache = &sync.Map{}

	return &K8sEnricher{
		redisClient: redisClient,
		k8sClient:   k8sClient,
		config:      config,
		cache:       cache,
	}
}

func (e *K8sEnricher) FetchK8sState(myNode string) error {
	fmt.Printf("fetching k8s state from %s/api/v1/k8s/pods?node=%s\n", e.config.K8sWatcher.URL, myNode)
	resp, err := e.k8sClient.Get(fmt.Sprintf("%s/api/v1/k8s/pods?node=%s", e.config.K8sWatcher.URL, myNode))

	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var state K8sStateResponse
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return err
	}

	// Update cache
	for _, entity := range state.Entities {

		if entity.Pod.NodeName == myNode {
			for _, container := range entity.Pod.Containers {
				id, err := matchContainerID(container.Id)
				if err == nil {
					// Create new pod with only the matching container
					filteredPod := entity.Pod                 // Copy the pod
					filteredPod.Containers = []K8sContainer{} // Reset containers

					// Find and add only the matching container
					for _, c := range entity.Pod.Containers {
						if matchId, _ := matchContainerID(c.Id); matchId == id {
							filteredPod.Containers = append(filteredPod.Containers, c)
							break
						}
					}
					e.cache.Store(id, &filteredPod)
				}
			}
		}
	}
	//print cache
	e.cache.Range(func(key, value interface{}) bool {

		load, _ := e.cache.Load(key)
		loadPod := load.(*K8sPod)
		log.Debugf("key: %s value: %s", key, loadPod)
		return true
	})

	return nil
}

func (e *K8sEnricher) SubscribeToRedis(ctx context.Context, myNode string) error {
	pubsub := e.redisClient.Subscribe(ctx, e.config.Redis.Channel)
	if pubsub == nil {
		log.Fatalf("Failed to subscribe to channel %s", e.config.Redis.Channel)
	}

	defer pubsub.Close()

	// Should be *Subscription, but others are possible if other actions have been

	for {
		msg, err := pubsub.ReceiveMessage(ctx)
		if err != nil {
			panic(err)
		}

		var event K8sRedisEvent
		if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
			log.Debugf("Failed to parse event: %v", err)
			continue
		}

		if event.Modified != nil {
			if event.Modified.Pod.NodeName == myNode {
				for _, container := range event.Modified.Pod.Containers {
					id, err := matchContainerID(container.Id)
					if err == nil {
						// Create new pod with only the matching container
						filteredPod := event.Modified.Pod         // Copy the pod
						filteredPod.Containers = []K8sContainer{} // Reset containers

						// Find and add only the matching container
						for _, c := range event.Modified.Pod.Containers {
							if matchId, _ := matchContainerID(c.Id); matchId == id {
								filteredPod.Containers = append(filteredPod.Containers, c)
								break
							}
						}
						e.cache.Store(id, &filteredPod)
					}
					log.Debugf("modified event  %s", event.Modified.Pod)

				}
			}
		} else if event.Deleted != nil {
			if event.Deleted.Pod.NodeName == myNode {
				for _, container := range event.Deleted.Pod.Containers {

					id, err := matchContainerID(container.Id)
					if err == nil {
						e.cache.Delete(id)
					}
					log.Debugf("delete event %s", event.Deleted.Pod)
				}

			}
		}
	}
	return nil

}

func K8sWatcher(nodeName string, cfg Config) *sync.Map {

	config := K8sEnricherConfig{
		Redis: RedisConfig{
			Host:    cfg.RedisHost,
			Port:    cfg.RedisPort,
			Channel: "k8s-events",
		},
		K8sWatcher: K8sWatcherConfig{
			URL:           cfg.K8SWatcherHost,
			Timeout:       10 * time.Second,
			RetryAttempts: 3,
		},
	}

	enricher := NewK8sEnricher(config)

	// Fetch initial state
	if err := enricher.FetchK8sState(nodeName); err != nil {
		log.Fatalf("Failed to fetch k8s state: %v", err)
	}

	// Subscribe to Redis
	go func() {
		ctx := context.Background()
		if err := enricher.SubscribeToRedis(ctx, nodeName); err != nil {
			log.Fatalf("Failed to subscribe to redis: %v", err)
		}
	}()
	return enricher.cache
}
