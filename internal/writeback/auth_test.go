package writeback

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"testing"
	"time"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	gossh "golang.org/x/crypto/ssh"

	v1alpha1 "github.com/jakeschurch/argocd-tag-updater/api/v1alpha1"
)

func testSSHKey(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block)
}

func TestAuthSelection(t *testing.T) {
	sshKey := testSSHKey(t)
	const sshRepo = "ssh://git@forgejo.example:22/jake/homelab.git"
	tests := []struct {
		name     string
		repo     string
		creds    Credentials
		wantURL  string
		wantUser string
		wantPass string
		wantSSH  bool
		wantNone bool
	}{
		{
			name: "ssh key stays the default and keeps ssh url", repo: sshRepo,
			creds:   Credentials{SSHPrivateKey: sshKey, InsecureIgnoreHostKey: true},
			wantURL: sshRepo, wantSSH: true,
		},
		{
			name: "ssh key wins over token", repo: sshRepo,
			creds:   Credentials{SSHPrivateKey: sshKey, InsecureIgnoreHostKey: true, Token: "t", Username: "robot"},
			wantURL: sshRepo, wantSSH: true,
		},
		{
			name: "token with username authenticates as robot", repo: "https://forgejo.example/jake/homelab.git",
			creds:   Credentials{Token: "tok", Username: "margo-robot"},
			wantURL: "https://forgejo.example/jake/homelab.git", wantUser: "margo-robot", wantPass: "tok",
		},
		{
			name: "token without username keeps x-access-token", repo: "https://forgejo.example/jake/homelab.git",
			creds:   Credentials{Token: "tok"},
			wantURL: "https://forgejo.example/jake/homelab.git", wantUser: "x-access-token", wantPass: "tok",
		},
		{
			name: "token with ssh repo url is rewritten to https without ssh port", repo: sshRepo,
			creds:   Credentials{Token: "tok", Username: "robot"},
			wantURL: "https://forgejo.example/jake/homelab.git", wantUser: "robot", wantPass: "tok",
		},
		{
			name: "http repo url is left alone for in-cluster forgejo", repo: "http://forgejo-http.forgejo.svc:3000/jake/homelab.git",
			creds:   Credentials{Token: "tok", Username: "robot"},
			wantURL: "http://forgejo-http.forgejo.svc:3000/jake/homelab.git", wantUser: "robot", wantPass: "tok",
		},
		{
			name: "username and password still work", repo: "https://forgejo.example/jake/homelab.git",
			creds:   Credentials{Username: "u", Password: "p"},
			wantURL: "https://forgejo.example/jake/homelab.git", wantUser: "u", wantPass: "p",
		},
		{
			name: "no credentials is anonymous", repo: "https://forgejo.example/jake/homelab.git",
			wantURL: "https://forgejo.example/jake/homelab.git", wantNone: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := Writer{Spec: v1alpha1.WriteBackSpec{Repo: tt.repo}, Credentials: tt.creds}
			auth, url, cleanup, err := w.auth()
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if url != tt.wantURL {
				t.Errorf("url = %q, want %q", url, tt.wantURL)
			}
			switch {
			case tt.wantNone:
				if auth != nil {
					t.Errorf("auth = %#v, want nil", auth)
				}
			case tt.wantSSH:
				if _, ok := auth.(*gitssh.PublicKeys); !ok {
					t.Errorf("auth = %T, want *ssh.PublicKeys", auth)
				}
			default:
				basic, ok := auth.(*githttp.BasicAuth)
				if !ok {
					t.Fatalf("auth = %T, want *http.BasicAuth", auth)
				}
				if basic.Username != tt.wantUser || basic.Password != tt.wantPass {
					t.Errorf("basic auth = %q/%q, want %q/%q", basic.Username, basic.Password, tt.wantUser, tt.wantPass)
				}
			}
		})
	}
}

func TestCommitSignature(t *testing.T) {
	when := time.Unix(1700000000, 0)
	httpsRepo := "https://forgejo.example:8443/jake/homelab.git"
	tests := []struct {
		name      string
		spec      v1alpha1.WriteBackSpec
		creds     Credentials
		wantName  string
		wantEmail string
	}{
		{
			name: "ssh keeps historical identity", spec: v1alpha1.WriteBackSpec{Repo: "git@forgejo.example:jake/homelab.git"},
			creds:    Credentials{SSHPrivateKey: []byte("k")},
			wantName: "argocd-tag-updater", wantEmail: "argocd-tag-updater@localhost",
		},
		{
			name: "token with username uses robot identity", spec: v1alpha1.WriteBackSpec{Repo: httpsRepo},
			creds:    Credentials{Token: "t", Username: "margo-robot"},
			wantName: "margo-robot", wantEmail: "margo-robot@noreply.forgejo.example",
		},
		{
			name: "robot identity from ssh-style repo url", spec: v1alpha1.WriteBackSpec{Repo: "ssh://git@forgejo.example:22/jake/homelab.git"},
			creds:    Credentials{Token: "t", Username: "margo-robot"},
			wantName: "margo-robot", wantEmail: "margo-robot@noreply.forgejo.example",
		},
		{
			name: "username/password keeps default author", spec: v1alpha1.WriteBackSpec{Repo: httpsRepo},
			creds:    Credentials{Username: "u", Password: "p"},
			wantName: "argocd-tag-updater", wantEmail: "argocd-tag-updater@localhost",
		},
		{
			name: "token without username keeps default", spec: v1alpha1.WriteBackSpec{Repo: httpsRepo},
			creds:    Credentials{Token: "t"},
			wantName: "argocd-tag-updater", wantEmail: "argocd-tag-updater@localhost",
		},
		{
			name:     "explicit commitAuthor overrides robot identity",
			spec:     v1alpha1.WriteBackSpec{Repo: httpsRepo, CommitAuthor: v1alpha1.CommitAuthorSpec{Name: "Margo Bot", Email: "bot@example.com"}},
			creds:    Credentials{Token: "t", Username: "margo-robot"},
			wantName: "Margo Bot", wantEmail: "bot@example.com",
		},
		{
			name:     "partial commitAuthor overrides only that field",
			spec:     v1alpha1.WriteBackSpec{Repo: httpsRepo, CommitAuthor: v1alpha1.CommitAuthorSpec{Email: "bot@example.com"}},
			creds:    Credentials{Token: "t", Username: "margo-robot"},
			wantName: "margo-robot", wantEmail: "bot@example.com",
		},
		{
			name:     "commitAuthor applies to ssh too",
			spec:     v1alpha1.WriteBackSpec{Repo: "git@forgejo.example:jake/homelab.git", CommitAuthor: v1alpha1.CommitAuthorSpec{Name: "Custom"}},
			creds:    Credentials{SSHPrivateKey: []byte("k")},
			wantName: "Custom", wantEmail: "argocd-tag-updater@localhost",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig := (Writer{Spec: tt.spec, Credentials: tt.creds}).signature(when)
			if sig.Name != tt.wantName || sig.Email != tt.wantEmail || !sig.When.Equal(when) {
				t.Errorf("signature = %q <%s> @%v, want %q <%s>", sig.Name, sig.Email, sig.When, tt.wantName, tt.wantEmail)
			}
		})
	}
}

func TestHostOf(t *testing.T) {
	for repo, want := range map[string]string{
		"https://forgejo.example/a/b.git":          "forgejo.example",
		"http://forgejo-http.forgejo.svc:3000/a/b": "forgejo-http.forgejo.svc",
		"ssh://git@forgejo.example:22/a/b.git":     "forgejo.example",
		"git@forgejo.example:a/b.git":              "forgejo.example",
	} {
		if got := hostOf(repo); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", repo, got, want)
		}
	}
}
