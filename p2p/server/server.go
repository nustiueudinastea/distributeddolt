package server

import (
	"context"
	"errors"

	p2pgrpc "github.com/birros/go-libp2p-grpc"
	"github.com/nustiueudinastea/doltswarm"
	"github.com/protosio/doltswarmdemo/p2p/proto"
	"google.golang.org/grpc"
)

var _ proto.PingerServer = (*Server)(nil)
var _ proto.TesterServer = (*Server)(nil)

type ExternalDB interface {
	AddPeer(peerID string, conn *grpc.ClientConn) error
	RemovePeer(peerID string) error
	GetAllCommits() ([]doltswarm.Commit, error)
	ExecAndCommit(query string, commitMsg string) (string, error)
	GetLastCommit(branch string) (doltswarm.Commit, error)
}

type Server struct {
	DB ExternalDB
}

func (s *Server) Ping(ctx context.Context, req *proto.PingRequest) (*proto.PingResponse, error) {
	_, ok := p2pgrpc.RemotePeerFromContext(ctx)
	if !ok {
		return nil, errors.New("no AuthInfo in context")
	}

	res := &proto.PingResponse{
		Pong: "Ping: " + req.Ping + "!",
	}
	return res, nil
}

func (s *Server) ExecSQL(ctx context.Context, req *proto.ExecSQLRequest) (*proto.ExecSQLResponse, error) {
	commit, err := s.DB.ExecAndCommit(req.Statement, req.Msg)
	if err != nil {
		return nil, err
	}
	return &proto.ExecSQLResponse{Result: "", Commit: commit}, nil
}

func (s *Server) GetAllCommits(context.Context, *proto.GetAllCommitsRequest) (*proto.GetAllCommitsResponse, error) {
	commits, err := s.DB.GetAllCommits()
	if err != nil {
		return nil, err
	}

	res := &proto.GetAllCommitsResponse{}
	for _, commit := range commits {
		res.Commits = append(res.Commits, commit.Hash)
	}

	return res, nil
}

func (s *Server) GetHead(context.Context, *proto.GetHeadRequest) (*proto.GetHeadResponse, error) {
	commit, err := s.DB.GetLastCommit("main")
	if err != nil {
		return nil, err
	}
	return &proto.GetHeadResponse{Commit: commit.Hash}, nil
}
