package reader

import (
 "context"
 "fmt"
 "testing"
 "time"

 "github.com/wangjohn/agent-archive/internal/listingindex"
 "github.com/wangjohn/agent-archive/internal/storage"
)

type indexedCountingStore struct {
 *countingStore
 pages int
}

func (s *indexedCountingStore) ListPage(ctx context.Context,prefix,continuation string,limit int32)(storage.ObjectPage,error){
 s.pages++
 return s.MemoryStore.ListPage(ctx,prefix,continuation,limit)
}

func TestListRecentStopsAfterVerifiedLimitAndKeepsHonestCount(t *testing.T){
 ctx:=context.Background()
 store:=&indexedCountingStore{countingStore:newCountingStore()}
 for i:=0;i<300;i++ {
  putSession(t,store,"codex",fmt.Sprintf("%032x",i+1),baseTime.Add(time.Duration(i)*time.Minute))
 }
 if got,err:=RebuildIndex(ctx,store,"sessions");err!=nil||got!=300{t.Fatalf("rebuild=%d,%v",got,err)}
 store.reset()
 got,err:=ListRecent(ctx,store,"sessions",Filter{},50,ListOptions{})
 if err!=nil{t.Fatal(err)}
 if got.Complete||len(got.Sessions)!=50||!got.Sessions[0].CapturedAt.Equal(baseTime.Add(299*time.Minute)){t.Fatalf("recent=%+v",got)}
 _,gets:=store.counts()
 if store.pages!=1||len(gets)!=52{t.Fatalf("remote work: pages=%d gets=%d",store.pages,len(gets))}
 all,err:=ListRecent(ctx,store,"sessions",Filter{},0,ListOptions{})
 if err!=nil||!all.Complete||all.TotalMatched!=300{t.Fatalf("full result: count=%d complete=%v err=%v",all.TotalMatched,all.Complete,err)}
}

func TestListRecentIgnoresStaleIndexAndDeletedMetadata(t *testing.T){
 ctx:=context.Background()
 store:=&indexedCountingStore{countingStore:newCountingStore()}
 old:=putSession(t,store,"codex",fmt.Sprintf("%032x",1),baseTime)
 putSession(t,store,"codex",fmt.Sprintf("%032x",2),baseTime.Add(time.Hour))
 if _,err:=RebuildIndex(ctx,store,"sessions");err!=nil{t.Fatal(err)}
 // A replacement sidecar invalidates its old hash. Until the matching hint
 // is published, the old revision must never appear.
 putSession(t,store,"codex",fmt.Sprintf("%032x",1),baseTime.Add(2*time.Hour))
 got,err:=ListRecent(ctx,store,"sessions",Filter{},50,ListOptions{})
 if err!=nil||len(got.Sessions)!=1{t.Fatalf("stale result=%d,%v",len(got.Sessions),err)}
 if _,err:=RebuildIndex(ctx,store,"sessions");err!=nil{t.Fatal(err)}
 got,err=ListRecent(ctx,store,"sessions",Filter{},50,ListOptions{})
 if err!=nil||len(got.Sessions)!=2||got.Sessions[0].SessionID!=fmt.Sprintf("%032x",1){t.Fatalf("rebuild result=%+v,%v",got,err)}
 if err:=store.Delete(ctx,old);err!=nil{t.Fatal(err)}
 got,err=ListRecent(ctx,store,"sessions",Filter{},50,ListOptions{})
 if err!=nil||len(got.Sessions)!=1{t.Fatalf("deleted result=%d,%v",len(got.Sessions),err)}
 if ready,err:=listingindex.Ready(ctx,store);err!=nil||!ready{t.Fatalf("ready=%v,%v",ready,err)}
}
