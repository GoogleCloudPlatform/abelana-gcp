// Copyright 2014 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package abelana

import (
	//    "fmt"

	"encoding/json"
	"net/http"
	"strings"
	"time"

	"appengine"
	"appengine/datastore"
	"appengine/delay"
	"appengine/socket"
	"appengine/urlfetch"

	auth "code.google.com/p/google-api-go-client/oauth2/v2"

	"github.com/garyburd/redigo/redis"
	"github.com/go-martini/martini"
)

////////////////////////////////////////////////////////////////////
const EnableBackdoor = true // FIXME(lesv) TEMPORARY BACKDOOR ACCESS
const enableStubs = true

////////////////////////////////////////////////////////////////////

// These things shouldn't be here, but there isn't a good place to get them at the moment.
const (
	authEmail         = "abelana-222@appspot.gserviceaccount.com"
	projectID         = "abelana-222"
	bucket            = "abelana-in"
	redisInt          = "10.240.85.221:6379"
	redisExt          = "23.251.150.167:6379"
	uploadRetries     = 5
	timelineBatchSize = 100
)

var delayFunc = delay.Func("test003", func(cx appengine.Context, x string) {
	cx.Infof("delay happened " + x)
})

var delayCopyImage = delay.Func("CopyImage001", CopyUserPhoto)
var delayAddPhoto = delay.Func("AddImage002", AddPhoto)
var delayAddFollower = delay.Func("AddFollower03", addTheFollower)

// User is the root structure for everything.  For RockStars, it will probably get too large to
// memcache, so we'll skip that for now.
type User struct {
	UserID      string
	DisplayName string
	Email       string
	Followers   []string
}

// Photo is how we keep images in Datastore
type Photo struct {
	PhotoID string
	Date    int64
}

// ToLike knows about who likes you.
type ToLike struct {
	UserID string
}

// Comment holds all comments
type Comment struct {
	FollowerID string `json:"followerid"`
	Text       string `json:"text"`
	Time       int64  `json:"time"`
}

// Comments returned from GetComments()
type Comments struct {
	Kind    string    `json:"kind"`
	Entries []Comment `json:"entries"`
}

// TLEntry holds timeline entries
type TLEntry struct {
	Created int64  `json:"created"`
	UserID  string `json:"userid"`
	Name    string `json:"name"`
	PhotoID string `json:"photoid"`
	Likes   int    `json:"likes"`
	ILike   bool   `json:"ilike"`
}

// Timeline the data the client sees.
type Timeline struct {
	Kind    string    `json:"kind"`
	Entries []TLEntry `json:"entries"`
}

// Follower holds information about our followers
type Follower struct {
	kind       string `json:"kind"`
	FollowerID string `json:"followerid"`
	Email      string `json:"email"`
	Name       string `json:"name"`
}

// Followers holds a list of our followers
type Followers struct {
	kind      string   `json:"kind"`
	Followers []string `json:"followerid"`
}

// ATOKJson is the json message for an Access Token (TEMPORARY - Until GitKit supports this)
type ATOKJson struct {
	Kind string `json:"kind"`
	Atok string `json:"atok"`
}

// Status is what we return if we have nothing to return
type Status struct {
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

// AppEngine middleware inserts a context where it's needed.
func AppEngine(c martini.Context, r *http.Request) {
	c.MapTo(appengine.NewContext(r), (*appengine.Context)(nil))
}

func init() {
	m := martini.Classic()
	m.Use(AppEngine)

	m.Get("/user/:gittok/login/:displayName/:photoUrl", Login)                 // => ATOKJson
	m.Get("/user/:atok/refresh", Aauth, Refresh)                               // => ATOKJson
	m.Get("/user/:atok/useful", Aauth, GetSecretKey)                           // => Status
	m.Delete("/user/:atok", Aauth, Wipeout)                                    // => Status
	m.Post("/user/:atok/facebook/:fbkey", Aauth, Import)                       // => Status
	m.Post("/user/:atok/plus/:plkey", Aauth, Import)                           // => Status
	m.Post("/user/:atok/yahoo/:ykey", Aauth, Import)                           // => Status
	m.Get("/user/:atok/follower", Aauth, GetFollowers)                         // => Followers
	m.Put("/user/:atok/follower/:followerid", Aauth, AddFollower)              // => Status
	m.Get("/user/:atok/follower/:followerid", Aauth, GetFollower)              // => Follower
	m.Put("/user/:atok/follower/:email", Aauth, Follow)                        // => Status
	m.Put("/user/atok/device/:regid", Aauth, Register)                         // => Status
	m.Delete("/user/:atok/device/:regid", Aauth, Unregister)                   // => Status
	m.Get("/user/:atok/timeline/:lastid", Aauth, GetTimeLine)                  // => Timeline
	m.Get("/user/:atok/profile/:lastid", Aauth, GetMyProfile)                  // => Timeline
	m.Get("/user/:atok/follower/:followerid/profile/:lastid", Aauth, FProfile) // => Timeline
	m.Post("/photo/:atok/:photoid/comment/:text", Aauth, SetPhotoComments)     // => Status
	m.Get("/photo/:atok/:photoid/comments", Aauth, GetPhotoComments)           // => Comments
	m.Put("/photo/:atok/:photoid/like", Aauth, Like)                           // => Status
	m.Delete("/photo/:atok/:photoid/like", Aauth, Unlike)                      // => Status
	m.Get("/photo/:atok/:photoid/flag", Aauth, Flag)                           // => Status

	m.Post("/photopush/:superid", PostPhoto) // "ok"

	if EnableBackdoor {
		m.Get("/les", Test)
		m.Get("/user/:gittok/login", Login)
	}

	tokenInit()

	http.Handle("/", m)
	redisInit()
}

// Test does magic of the moment
func Test(cx appengine.Context) string {
	cx.Infof("Test...")
	delayFunc.Call(cx, "hello world")
	return `ok`
}

// replyJSON Given an object, convert to JSON and reply with it
func replyJSON(w http.ResponseWriter, v interface{}) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Add("Content-Type", "application/json")
	_, err = w.Write(b)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func replyOk(w http.ResponseWriter) {
	st := &Status{"abelana#status", "ok"}
	replyJSON(w, st)
}

///////////////////////////////////////////////////////////////////////////////////////////////////
// Timeline
///////////////////////////////////////////////////////////////////////////////////////////////////

// GetTimeLine - get the timeline for the user (token) : TlResp
func GetTimeLine(cx appengine.Context, at Access, w http.ResponseWriter) {
	timeline := []TLEntry{}

	if !enableStubs {

	} else {
		t := time.Now().Unix()
		timeline = []TLEntry{
			TLEntry{t - 200, "00001", "Les", "0001", 1, false},
			TLEntry{t - 1000, "00001", "Les", "0002", 99, false},
			TLEntry{t - 2500, "00001", "Les", "0003", 0, false},
			TLEntry{t - 6040, "00001", "Les", "0004", 3, true},
			TLEntry{t - 7500, "00001", "Les", "0005", 1, true},
			TLEntry{t - 9300, "00001", "Les", "0006", 99, false},
			TLEntry{t - 10200, "00001", "Les", "0007", 0, false},
			TLEntry{t - 47003, "00001", "Les", "0008", 3, false},
			TLEntry{t - 53002, "00001", "Les", "0009", 1, true},
			TLEntry{t - 54323, "00001", "Les", "0010", 99, false},
			TLEntry{t - 56112, "00001", "Les", "0011", 0, false},
			TLEntry{t - 58243, "00001", "Les", "0004", 3, false},
			TLEntry{t - 80201, "00001", "Les", "0001", 1, true},
			TLEntry{t - 80500, "00001", "Les", "0002", 99, true},
			TLEntry{t - 81200, "00001", "Les", "0003", 0, false},
			TLEntry{t - 89302, "00001", "Les", "0005", 3, false},
			TLEntry{t - 91200, "00001", "Les", "0007", 1, false},
			TLEntry{t - 92343, "00001", "Les", "0006", 99, false},
			TLEntry{t - 93233, "00001", "Les", "0011", 0, false},
			TLEntry{t - 94322, "00001", "Les", "0009", 3, false},
			TLEntry{t - 95323, "00001", "Les", "0002", 99, false},
			TLEntry{t - 96734, "00001", "Les", "0003", 0, false},
			TLEntry{t - 98033, "00001", "Les", "0004", 3, false},
			TLEntry{t - 99334, "00001", "Les", "0005", 1, false},
			TLEntry{t - 99993, "00001", "Les", "0006", 99, false},
			TLEntry{t - 102304, "00001", "Les", "0007", 0, false},
			TLEntry{t - 102750, "00001", "Les", "0008", 3, false},
			TLEntry{t - 104333, "00001", "Les", "0009", 1, false},
			TLEntry{t - 105323, "00001", "Les", "0010", 99, false},
			TLEntry{t - 107323, "00001", "Les", "0011", 0, false},
			TLEntry{t - 109323, "00001", "Les", "0004", 3, false},
			TLEntry{t - 110000, "00001", "Les", "0001", 1, false},
			TLEntry{t - 110133, "00001", "Les", "0002", 99, false},
			TLEntry{t - 113444, "00001", "Les", "0003", 0, false},
			TLEntry{t - 122433, "00001", "Les", "0005", 3, false},
			TLEntry{t - 125320, "00001", "Les", "0007", 1, false},
			TLEntry{t - 125325, "00001", "Les", "0006", 99, false},
			TLEntry{t - 127555, "00001", "Les", "0011", 0, false},
			TLEntry{t - 128333, "00001", "Les", "0009", 3, false},
			TLEntry{t - 173404, "00001", "Les", "0005", 21, false}}
	}
	tl := &Timeline{"abelana#timeline", timeline}
	replyJSON(w, tl)
}

// GetMyProfile - Get my entries only (token) : TlResp
func GetMyProfile(cx appengine.Context, at Access, w http.ResponseWriter) {
	timeline := []TLEntry{}

	if !enableStubs {
		k1 := datastore.NewKey(cx, "User", at.ID(), 0, nil)
		user := &User{}
		err := datastore.Get(cx, k1, &user)
		if err != nil {
			cx.Errorf("GetMyProfile %v %v", at.ID(), err)
			replyOk(w)
		}

	} else {
		t := time.Now().Unix()
		timeline = []TLEntry{
			TLEntry{t - 99993, "00001", "Les", "0006", 99, true},
			TLEntry{t - 102304, "00001", "Les", "0007", 0, false},
			TLEntry{t - 102750, "00001", "Les", "0008", 3, false},
			TLEntry{t - 104333, "00001", "Les", "0009", 1, false},
			TLEntry{t - 105323, "00001", "Les", "0010", 9, false},
			TLEntry{t - 107323, "00001", "Les", "0011", 0, false},
			TLEntry{t - 109323, "00001", "Les", "0004", 3, false},
			TLEntry{t - 110000, "00001", "Les", "0001", 1, false},
			TLEntry{t - 110133, "00001", "Les", "0002", 99, true},
			TLEntry{t - 113444, "00001", "Les", "0003", 0, false},
			TLEntry{t - 122433, "00001", "Les", "0005", 3, false},
			TLEntry{t - 125320, "00001", "Les", "0007", 1, false},
			TLEntry{t - 125325, "00001", "Les", "0006", 99, false},
			TLEntry{t - 127555, "00001", "Les", "0011", 0, false},
			TLEntry{t - 128333, "00001", "Les", "0009", 3, false},
			TLEntry{t - 173404, "00001", "Les", "0005", 21, false}}
	}
	tl := &Timeline{"abelana#timeline", timeline}
	replyJSON(w, tl)
}

// FProfile - Get a specific followers entries only (TlfReq) : TlResp
func FProfile(cx appengine.Context, at Access, p martini.Params, w http.ResponseWriter) {
	timeline := []TLEntry{}

	if !enableStubs {
		k1 := datastore.NewKey(cx, "User", at.ID(), 0, nil)
		user := &User{}
		datastore.Get(cx, k1, &user)

	} else {
		t := time.Now().Unix()
		timeline = []TLEntry{
			TLEntry{t - 80500, "00001", "Les", "0002", 99, true},
			TLEntry{t - 81200, "00001", "Les", "0003", 0, false},
			TLEntry{t - 89302, "00001", "Les", "0005", 3, true},
			TLEntry{t - 91200, "00001", "Les", "0007", 1, false},
			TLEntry{t - 92343, "00001", "Les", "0006", 99, true},
			TLEntry{t - 93233, "00001", "Les", "0011", 0, false},
			TLEntry{t - 94322, "00001", "Les", "0009", 3, false},
			TLEntry{t - 95323, "00001", "Les", "0002", 99, false},
			TLEntry{t - 96734, "00001", "Les", "0003", 0, false},
			TLEntry{t - 98033, "00001", "Les", "0004", 3, false},
			TLEntry{t - 99334, "00001", "Les", "0005", 1, false},
			TLEntry{t - 99993, "00001", "Les", "0006", 99, false}}
	}
	tl := &Timeline{"abelana#timeline", timeline}
	replyJSON(w, tl)
}

// PostPhoto lets us know that we have a photo, we then tell both DataStore and Redis
func PostPhoto(cx appengine.Context, p martini.Params, w http.ResponseWriter, rq *http.Request) string {
	cx.Infof("PostPhoto %v", p["superid"])
	otok := rq.Header.Get("Authorization")
	if !appengine.IsDevAppServer() {
		ok, err := authorized(cx, otok)
		if !ok {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return ``
		}
	}
	s := strings.Split(p["superid"], ".")
	if len(s) == 3 { // We only need to call for userid.photoID.webp
		delayAddPhoto.Call(cx, p["superid"])
	}
	return `ok`
}

func authorized(cx appengine.Context, token string) (bool, error) {
	if fs := strings.Fields(token); len(fs) == 2 && fs[0] == "Bearer" {
		token = fs[1]
	} else {
		return false, nil
	}

	svc, err := auth.New(urlfetch.Client(cx))
	if err != nil {
		return false, err
	}
	tok, err := svc.Tokeninfo().Access_token(token).Do()
	if err != nil {
		return false, err
	}
	cx.Infof("  tok %v", tok)
	return tok.Email == authEmail, nil
}

///////////////////////////////////////////////////////////////////////////////////////////////////
// Import
///////////////////////////////////////////////////////////////////////////////////////////////////

// Import for Facebook / G+ / ... (xcred) : StatusResp
func Import(cx appengine.Context, at Access, p martini.Params, w http.ResponseWriter) {
	replyOk(w)
}

///////////////////////////////////////////////////////////////////////////////////////////////////
// Follower
///////////////////////////////////////////////////////////////////////////////////////////////////

// GetFollowers - A list of our followers (AToken) : FlResp
func GetFollowers(cx appengine.Context, at Access, p martini.Params, w http.ResponseWriter) {
	fl := &Followers{}

	if !enableStubs {
		k1 := datastore.NewKey(cx, "User", at.ID(), 0, nil)
		user := &User{}
		err := datastore.Get(cx, k1, &user)
		if err != nil {
			cx.Errorf("GetFollowers %v %v", at.ID(), err)
			replyOk(w)
		}
		fl = &Followers{"abelana#followerList", user.Followers}
	} else {
		fl = &Followers{"abelana#followerList", []string{"00001", "12730648828453578083"}}
	}
	replyJSON(w, fl)
}

// GetFollower -- find out about someone  : Follower
func GetFollower(cx appengine.Context, at Access, p martini.Params, w http.ResponseWriter) {
	f := &Follower{}

	if !enableStubs {
		k1 := datastore.NewKey(cx, "User", p["followerid"], 0, nil)
		user := &User{}
		err := datastore.Get(cx, k1, &user)
		if err != nil {
			cx.Errorf("GetFollower %v %v %v", p["followerid"], p["followerid"], err)
			replyOk(w)
		}
		f = &Follower{"abelana#follower", user.UserID, user.Email, user.DisplayName}
	} else {
		f = &Follower{"abelana#follower", "00001", "lesv@abelana-app.com", "Les Vogel"}
	}
	replyJSON(w, f)
}

// AddFollower - will tell us about a new possible follower (FrReq) : Status
func AddFollower(cx appengine.Context, at Access, p martini.Params, w http.ResponseWriter) {
	k1 := datastore.NewKey(cx, "User", at.ID(), 0, nil)
	user := &User{}
	err := datastore.Get(cx, k1, &user)
	if err != nil {
		cx.Errorf("AddFollower %v %v %v", at.ID(), p["followerid"], err)
		replyOk(w)
	}
	sl := user.Followers
	if len(sl) == cap(sl) {
		newSl := make([]string, len(sl), len(sl)+1)
		copy(newSl, sl)
		sl = newSl
	}
	user.Followers = sl[0 : len(sl)+1]
	user.Followers[len(sl)] = p["followerid"]
	_, err = datastore.Put(cx, k1, &user)
	delayAddFollower.Call(cx, at.ID(), p["followerid"])
	replyOk(w)
}

// Follow will see if we can follow the user, given their email
func Follow(cx appengine.Context, at Access, p martini.Params, w http.ResponseWriter) {
	replyOk(w)
}

///////////////////////////////////////////////////////////////////////////////////////////////////
// Photo
///////////////////////////////////////////////////////////////////////////////////////////////////

// SetPhotoComments allows the users voice to be heard (PhotoComment) : Status
func SetPhotoComments(cx appengine.Context, at Access, p martini.Params, w http.ResponseWriter) {
	s := strings.Split(p["photoid"], ".")
	tod := time.Now().UTC().Unix()
	k1 := datastore.NewKey(cx, "User", s[0], 0, nil)
	k2 := datastore.NewKey(cx, "Photo", s[1], 0, k1)
	k3 := datastore.NewKey(cx, "Comment", "", tod, k2)
	c := &Comment{at.ID(), p["text"], tod}
	_, err := datastore.Put(cx, k3, c)
	if err != nil {
		cx.Errorf("SetPhotoComments: %v %v", k3, err)
	}
	replyOk(w)
}

// GetPhotoComments will get the comments given a photoid
func GetPhotoComments(cx appengine.Context, at Access, p martini.Params, w http.ResponseWriter) {
	var c []Comment

	s := strings.Split(p["photoid"], ".")
	k1 := datastore.NewKey(cx, "User", s[0], 0, nil)
	k2 := datastore.NewKey(cx, "Photo", s[1], 0, k1)

	q := datastore.NewQuery("Comment").Ancestor(k2).Order("Time")
	_, err := q.GetAll(cx, &c)
	if err != nil {
		cx.Errorf("GetPhotoComments %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cl := &Comments{"abelana#comments", c}
	replyJSON(w, cl)
}

// Like let's the user tell of their joy (Photo) : Status
func Like(cx appengine.Context, at Access, p martini.Params, w http.ResponseWriter) {
	s := strings.Split(p["photoid"], ".")

	hc, err := socket.Dial(cx, "tcp", server)
	if err != nil {
		cx.Errorf("Like Dial %v", err)
		return
	}
	defer hc.Close()
	conn := redis.NewConn(hc, 0, 0) // TODO 0 TO's for now
	photo := s[0] + "." + s[1]
	_, err = redis.Int(conn.Do("INCR", photo))
	if err != nil && err != redis.ErrNil {
		cx.Errorf("Like %v", err)
	}

	k1 := datastore.NewKey(cx, "User", s[0], 0, nil)
	k2 := datastore.NewKey(cx, "Photo", s[1], 0, k1)
	k3 := datastore.NewKey(cx, "Like", at.ID(), 0, k2)
	l := &ToLike{at.ID()}
	_, err = datastore.Put(cx, k3, l)
	if err != nil {
		cx.Errorf("Like: %v %v", k3, err)
	}
	replyOk(w)
}

// Unlike let's the user recind their +1 (Photo) : Status
func Unlike(cx appengine.Context, at Access, p martini.Params, w http.ResponseWriter) {
	s := strings.Split(p["photoid"], ".")
	k1 := datastore.NewKey(cx, "User", s[0], 0, nil)
	k2 := datastore.NewKey(cx, "Photo", s[1], 0, k1)
	k3 := datastore.NewKey(cx, "Like", at.ID(), 0, k2)
	err := datastore.Delete(cx, k3)
	if err != nil {
		cx.Errorf("Unlike: %v %v", k3, err)
		replyOk(w)
		return
	}

	hc, err := socket.Dial(cx, "tcp", server)
	if err != nil {
		cx.Errorf("Unlike Dial %v", err)
		return
	}
	defer hc.Close()
	conn := redis.NewConn(hc, 0, 0) // TODO 0 TO's for now
	photo := s[0] + "." + s[1]
	_, err = redis.Int(conn.Do("DECR", photo))
	if err != nil && err != redis.ErrNil {
		cx.Errorf("Like %v", err)
	}
	replyOk(w)
}

// Flag will bring this to the administrators attention.
func Flag(cx appengine.Context, at Access, p martini.Params, w http.ResponseWriter) {
	replyOk(w)
}

///////////////////////////////////////////////////////////////////////////////////////////////////
// Management
///////////////////////////////////////////////////////////////////////////////////////////////////

// Wipeout will erase all data you are working on. (Atok) : Status
func Wipeout(cx appengine.Context, at Access, p martini.Params, w http.ResponseWriter) {
	replyOk(w)
}

// Register will start GCM messages to your device (GCMReq) : Status
func Register(cx appengine.Context, at Access, p martini.Params, w http.ResponseWriter) {
	replyOk(w)
}

// Unregister will stop GCM messages from going to your device (GCMReq) : Status
func Unregister(cx appengine.Context, at Access, p martini.Params, w http.ResponseWriter) {
	replyOk(w)
}
