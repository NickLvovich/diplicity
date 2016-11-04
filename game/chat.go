package game

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/zond/diplicity/auth"
	"github.com/zond/godip/variants"
	"golang.org/x/net/context"
	"google.golang.org/appengine"
	"google.golang.org/appengine/datastore"

	. "github.com/zond/goaeoas"
	dip "github.com/zond/godip/common"
)

const (
	messageKind = "Message"
	channelKind = "Channel"
)

type Nations []dip.Nation

func (n *Nations) FromString(s string) {
	parts := strings.Split(s, ",")
	*n = make(Nations, len(parts))
	for i := range parts {
		(*n)[i] = dip.Nation(parts[i])
	}
}

func (n Nations) Includes(m dip.Nation) bool {
	for i := range n {
		if n[i] == m {
			return true
		}
	}
	return false
}

func (n Nations) Len() int {
	return len(n)
}

func (n Nations) Less(i, j int) bool {
	return n[i] < n[j]
}

func (n Nations) Swap(i, j int) {
	n[i], n[j] = n[j], n[i]
}

func (n Nations) String() string {
	slice := make([]string, len(n))
	for i := range n {
		slice[i] = string(n[i])
	}
	return strings.Join(slice, ",")
}

type Channels []Channel

func (c Channels) Item(r Request, gameID *datastore.Key) *Item {
	channelItems := make(List, len(c))
	for i := range c {
		channelItems[i] = c[i].Item(r)
	}
	channelsItem := NewItem(channelItems).SetName("channels").SetDesc([][]string{
		[]string{
			"Lazy channels",
			"Channels are created lazily when messages are created for previously non existing channels.",
			"This means that you can write messages to combinations of nations not currently represented by a channel listed here, and the channel will simply be created for you.",
		},
		[]string{
			"Counters",
			"Channels tell you how many messages they have, and if you provide the `since` query parameter they will even tell you how many new messages they have received since then.",
		},
	}).AddLink(r.NewLink(Link{
		Rel:         "self",
		Route:       ListChannelsRoute,
		RouteParams: []string{"game_id", gameID.Encode()},
	}))
	channelsItem.AddLink(r.NewLink(MessageResource.Link("message", Create, []string{"game_id", gameID.Encode()})))
	return channelsItem
}

type NMessagesSince struct {
	Since     time.Time
	NMessages int
}

type Channel struct {
	GameID         *datastore.Key
	Members        Nations
	NMessages      int
	NMessagesSince NMessagesSince `datastore:"-" json:",omitempty"`
}

func (c *Channel) Item(r Request) *Item {
	sort.Sort(c.Members)
	channelItem := NewItem(c).SetName(c.Members.String())
	channelItem.AddLink(r.NewLink(Link{
		Rel:         "messages",
		Route:       ListMessagesRoute,
		RouteParams: []string{"game_id", c.GameID.Encode(), "channel_members", c.Members.String()},
	}))
	return channelItem
}

func ChannelID(ctx context.Context, gameID *datastore.Key, members Nations) (*datastore.Key, error) {
	if gameID == nil || len(members) < 2 {
		return nil, fmt.Errorf("channels must have games and > 1 members")
	}
	if gameID.IntID() == 0 {
		return nil, fmt.Errorf("gameIDs must have int IDs")
	}
	return datastore.NewKey(ctx, channelKind, fmt.Sprintf("%d:%s", gameID.IntID(), members.String()), 0, nil), nil
}

func (c *Channel) ID(ctx context.Context) (*datastore.Key, error) {
	return ChannelID(ctx, c.GameID, c.Members)
}

func (c *Channel) CountSince(ctx context.Context, since time.Time) error {
	channelID, err := ChannelID(ctx, c.GameID, c.Members)
	if err != nil {
		return err
	}
	count, err := datastore.NewQuery(messageKind).Ancestor(channelID).Filter("CreatedAt>", since).Count(ctx)
	if err != nil {
		return err
	}
	c.NMessagesSince.Since = since
	c.NMessagesSince.NMessages = count
	return nil
}

var MessageResource = &Resource{
	Create:     createMessage,
	CreatePath: "/Game/{game_id}/Messages",
}

type Messages []Message

func (m Messages) Item(r Request, gameID *datastore.Key, channelMembers Nations) *Item {
	messageItems := make(List, len(m))
	for i := range m {
		messageItems[i] = m[i].Item(r)
	}
	messagesItem := NewItem(messageItems).SetName("messages").SetDesc([][]string{
		[]string{
			"Limiting messages",
			"Messages normally contain all messages for the chosen channel, but if you provide a `since` query parameter they will only contain new messages since that time.",
		},
	}).AddLink(r.NewLink(Link{
		Rel:         "self",
		Route:       ListMessagesRoute,
		RouteParams: []string{"game_id", gameID.Encode(), "channel_members", channelMembers.String()},
	}))
	return messagesItem
}

type Message struct {
	ID             *datastore.Key `datastore:"-"`
	GameID         *datastore.Key
	ChannelMembers Nations `methods:"POST"`
	Sender         dip.Nation
	Body           string `methods:"POST"`
	CreatedAt      time.Time
}

func (m *Message) Item(r Request) *Item {
	return NewItem(m).SetName(string(m.Sender))
}

func createMessage(w ResponseWriter, r Request) (*Message, error) {
	ctx := appengine.NewContext(r.Req())

	user, ok := r.Values()["user"].(*auth.User)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return nil, nil
	}

	gameID, err := datastore.DecodeKey(r.Vars()["game_id"])
	if err != nil {
		return nil, err
	}

	memberID, err := MemberID(ctx, gameID, user.Id)
	if err != nil {
		return nil, err
	}

	game := &Game{}
	member := &Member{}
	err = datastore.GetMulti(ctx, []*datastore.Key{gameID, memberID}, []interface{}{game, member})
	if err != nil {
		return nil, err
	}

	message := &Message{}
	if err := Copy(message, r, "POST"); err != nil {
		return nil, err
	}
	message.GameID = gameID
	message.Sender = member.Nation
	message.CreatedAt = time.Now()
	sort.Sort(message.ChannelMembers)

	if !message.ChannelMembers.Includes(member.Nation) {
		http.Error(w, "can only send messages to member channels", 403)
		return nil, nil
	}

	for _, channelMember := range message.ChannelMembers {
		if !Nations(variants.Variants[game.Variant].Nations).Includes(channelMember) {
			http.Error(w, "unknown channel member", 400)
			return nil, nil
		}
	}

	channelID, err := ChannelID(ctx, gameID, message.ChannelMembers)
	if err != nil {
		return nil, err
	}

	if err := datastore.RunInTransaction(ctx, func(ctx context.Context) error {
		channel := &Channel{}
		if err := datastore.Get(ctx, channelID, channel); err == datastore.ErrNoSuchEntity {
			channel.GameID = gameID
			channel.Members = message.ChannelMembers
			channel.NMessages = 0
		}
		if message.ID, err = datastore.Put(ctx, datastore.NewIncompleteKey(ctx, messageKind, channelID), message); err != nil {
			return err
		}
		channel.NMessages += 1
		_, err = datastore.Put(ctx, channelID, channel)
		return err
	}, &datastore.TransactionOptions{XG: false}); err != nil {
		return nil, err
	}

	return message, nil
}

func publicChannel(variant string) Nations {
	publicChannel := make(Nations, len(variants.Variants[variant].Nations))
	copy(publicChannel, variants.Variants[variant].Nations)
	sort.Sort(publicChannel)

	return publicChannel
}

func isPublic(variant string, members Nations) bool {
	public := publicChannel(variant)

	sort.Sort(members)

	if len(members) != len(public) {
		return false
	}

	for i := range public {
		if members[i] != public[i] {
			return false
		}
	}

	return true
}

func listMessages(w ResponseWriter, r Request) error {
	ctx := appengine.NewContext(r.Req())

	user, ok := r.Values()["user"].(*auth.User)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return nil
	}

	gameID, err := datastore.DecodeKey(r.Vars()["game_id"])
	if err != nil {
		return err
	}

	channelMembers := Nations{}
	channelMembers.FromString(r.Vars()["channel_members"])

	var since *time.Time
	if sinceParam := r.Req().URL.Query().Get("since"); sinceParam != "" {
		sinceTime, err := time.Parse(time.RFC3339, sinceParam)
		if err != nil {
			return err
		}
		since = &sinceTime
	}

	memberID, err := MemberID(ctx, gameID, user.Id)
	if err != nil {
		return err
	}

	var nation dip.Nation

	game := &Game{}
	member := &Member{}
	err = datastore.GetMulti(ctx, []*datastore.Key{gameID, memberID}, []interface{}{game, member})
	if err == nil {
		nation = member.Nation
	} else if merr, ok := err.(appengine.MultiError); ok {
		if merr[0] != nil {
			return merr[0]
		}
	} else {
		return err
	}

	if !channelMembers.Includes(nation) && !isPublic(game.Variant, channelMembers) {
		http.Error(w, "can only list member channels", 403)
		return nil
	}

	channelID, err := ChannelID(ctx, gameID, channelMembers)
	if err != nil {
		return err
	}

	messages := Messages{}
	q := datastore.NewQuery(messageKind).Ancestor(channelID)
	if since != nil {
		q = q.Filter("CreatedAt>", *since)
	}
	if _, err := q.Order("-CreatedAt").GetAll(ctx, &messages); err != nil {
		return err
	}

	w.SetContent(messages.Item(r, gameID, channelMembers))
	return nil
}

func listChannels(w ResponseWriter, r Request) error {
	ctx := appengine.NewContext(r.Req())

	user, ok := r.Values()["user"].(*auth.User)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return nil
	}

	gameID, err := datastore.DecodeKey(r.Vars()["game_id"])
	if err != nil {
		return err
	}

	memberID, err := MemberID(ctx, gameID, user.Id)
	if err != nil {
		return err
	}

	var since *time.Time
	if sinceParam := r.Req().URL.Query().Get("since"); sinceParam != "" {
		sinceTime, err := time.Parse(time.RFC3339, sinceParam)
		if err != nil {
			return err
		}
		since = &sinceTime
	}

	var nation dip.Nation

	game := &Game{}
	member := &Member{}
	err = datastore.GetMulti(ctx, []*datastore.Key{gameID, memberID}, []interface{}{game, member})
	if err == nil {
		nation = member.Nation
	} else if merr, ok := err.(appengine.MultiError); ok {
		if merr[0] != nil {
			return merr[0]
		}
	} else {
		return err
	}

	channels := Channels{}
	if nation == "" {
		channelID, err := ChannelID(ctx, gameID, publicChannel(game.Variant))
		if err != nil {
			return err
		}
		channel := &Channel{}
		if err := datastore.Get(ctx, channelID, channel); err == nil {
			channels = append(channels, *channel)
		} else if err != datastore.ErrNoSuchEntity {
			return err
		}
	} else {
		_, err = datastore.NewQuery(channelKind).Filter("GameID=", gameID).Filter("Members=", nation).GetAll(ctx, &channels)
		if err != nil {
			return err
		}
	}

	if since != nil {
		results := make(chan error)
		for i := range channels {
			go func(c *Channel) {
				results <- c.CountSince(ctx, *since)
			}(&channels[i])
		}
		merr := appengine.MultiError{}
		for _ = range channels {
			if err := <-results; err != nil {
				merr = append(merr, err)
			}
		}
		if len(merr) > 0 {
			return merr
		}
	} else {
		for i := range channels {
			channels[i].NMessagesSince.NMessages = channels[i].NMessages
		}
	}

	w.SetContent(channels.Item(r, gameID))
	return nil
}
