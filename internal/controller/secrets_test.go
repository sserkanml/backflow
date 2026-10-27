package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backflowv1alpha1 "github.com/sserkanml/backflow/api/v1alpha1"
	"github.com/sserkanml/backflow/internal/gitrepo"
)

// countingReader counts the Secret reads that reach the API server.
type countingReader struct {
	client.Reader
	mu    sync.Mutex
	reads int
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.Reader.Get(ctx, key, obj, opts...)
}

func (c *countingReader) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// forbiddingReader answers every read like an API server that does not let the
// operator read Secrets in the namespace.
type forbiddingReader struct{ client.Reader }

func (forbiddingReader) Get(_ context.Context, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, nil)
}

var _ = Describe("Secret client", func() {
	var (
		secret *corev1.Secret
		reader *countingReader
		now    time.Time
	)
	newClient := func(ttl time.Duration) client.Client {
		c := NewSecretClient(fake.NewClientBuilder().Build(), reader, ttl).(*secretClient)
		c.now = func() time.Time { return now }
		return c
	}
	key := types.NamespacedName{Namespace: "team-a", Name: "token"}

	BeforeEach(func() {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "token"}, Data: map[string][]byte{"token": []byte("one")}}
		reader = &countingReader{Reader: fake.NewClientBuilder().WithObjects(secret).Build()}
		now = time.Now()
	})

	It("reads a Secret by name from the reader, not from the wrapped client", func() {
		var got corev1.Secret
		Expect(newClient(time.Minute).Get(ctx, key, &got)).To(Succeed())
		Expect(string(got.Data["token"])).To(Equal("one"))
		Expect(reader.count()).To(Equal(1))
	})

	It("keeps a Secret for the TTL and reads it again afterwards", func() {
		c := newClient(30 * time.Second)
		var got corev1.Secret
		Expect(c.Get(ctx, key, &got)).To(Succeed())
		Expect(c.Get(ctx, key, &got)).To(Succeed())
		Expect(reader.count()).To(Equal(1), "the second read is served from memory")

		now = now.Add(31 * time.Second)
		updated := secret.DeepCopy()
		updated.Data["token"] = []byte("two")
		Expect(reader.Reader.(client.Client).Update(ctx, updated)).To(Succeed())
		Expect(c.Get(ctx, key, &got)).To(Succeed())
		Expect(string(got.Data["token"])).To(Equal("two"), "a rotated token is read after the TTL")
		Expect(reader.count()).To(Equal(2))
	})

	It("does not hand out the object kept in memory", func() {
		c := newClient(time.Minute)
		var got corev1.Secret
		Expect(c.Get(ctx, key, &got)).To(Succeed())
		got.Data["token"][0] = 'X'
		var again corev1.Secret
		Expect(c.Get(ctx, key, &again)).To(Succeed())
		Expect(string(again.Data["token"])).To(Equal("one"))
	})

	It("reads every time without a TTL", func() {
		c := newClient(0)
		var got corev1.Secret
		Expect(c.Get(ctx, key, &got)).To(Succeed())
		Expect(c.Get(ctx, key, &got)).To(Succeed())
		Expect(reader.count()).To(Equal(2))
	})

	It("forgets the Secrets of a namespace on request", func() {
		c := newClient(time.Hour)
		var got corev1.Secret
		Expect(c.Get(ctx, key, &got)).To(Succeed())
		forgetSecrets(c, "other")
		Expect(c.Get(ctx, key, &got)).To(Succeed())
		Expect(reader.count()).To(Equal(1))
		forgetSecrets(c, "team-a")
		Expect(c.Get(ctx, key, &got)).To(Succeed())
		Expect(reader.count()).To(Equal(2))
	})

	It("does not keep a Secret that could not be read", func() {
		c := newClient(time.Hour)
		var got corev1.Secret
		err := c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "missing"}, &got)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "missing"}, &got)).NotTo(Succeed())
		Expect(reader.count()).To(Equal(2))
	})

	It("never lists Secrets", func() {
		c := newClient(time.Minute)
		Expect(c.List(ctx, &corev1.SecretList{})).To(MatchError(errListSecrets))
		Expect(c.List(ctx, &corev1.SecretList{}, client.InNamespace("team-a"))).To(MatchError(errListSecrets))
		Expect(c.List(ctx, &corev1.ConfigMapList{})).To(Succeed(), "other kinds are not affected")
	})

	It("leaves other kinds to the wrapped client", func() {
		inner := fake.NewClientBuilder().WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "cm"}}).Build()
		c := NewSecretClient(inner, reader, time.Minute)
		Expect(c.Get(ctx, types.NamespacedName{Namespace: "team-a", Name: "cm"}, &corev1.ConfigMap{})).To(Succeed())
		Expect(reader.count()).To(BeZero())
	})
})

var _ = Describe("secretReadError", func() {
	It("names the namespace and the chart value for a Forbidden answer", func() {
		err := secretReadError(apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "token", nil), "team-a", "token")
		Expect(isSecretForbidden(err)).To(BeTrue())
		Expect(reasonOf(err)).To(Equal("SecretForbidden"))
		Expect(err.Error()).To(And(ContainSubstring(`"team-a"`), ContainSubstring("secretNamespaces"), ContainSubstring(`"token"`)))
	})

	It("passes other errors through", func() {
		other := apierrors.NewInternalError(context.Canceled)
		Expect(secretReadError(other, "team-a", "token")).To(BeIdenticalTo(other))
		Expect(isSecretForbidden(other)).To(BeFalse())
	})
})

var _ = Describe("ScmConnection with Secrets the operator cannot read", func() {
	const ns = "default"

	var conn *backflowv1alpha1.ScmConnection
	BeforeEach(func() {
		conn = &backflowv1alpha1.ScmConnection{
			ObjectMeta: metav1.ObjectMeta{Name: "forbidden-conn", Namespace: ns},
			Spec: backflowv1alpha1.ScmConnectionSpec{
				Provider: backflowv1alpha1.ScmProviderGitLab,
				URL:      "https://gitlab.example.com",
				TokenSecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "scm-token"}, Key: "token",
				},
			},
		}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())
	})
	AfterEach(func() { Expect(k8sClient.Delete(ctx, conn)).To(Succeed()) })

	It("is not Ready, with the reason SecretForbidden", func() {
		r := &ScmConnectionReconciler{Client: NewSecretClient(k8sClient, forbiddingReader{}, 0), Scheme: k8sClient.Scheme()}
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(conn)})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(scmRetryInterval))

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(conn), conn)).To(Succeed())
		ready := meta.FindStatusCondition(conn.Status.Conditions, "Ready")
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("SecretForbidden"))
		Expect(ready.Message).To(And(ContainSubstring(`namespace "default"`), ContainSubstring("secretNamespaces")))
	})

	It("picks up a rotated token at the next verification, which is due after the configured interval", func() {
		gitlab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("PRIVATE-TOKEN") != "good" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"username":"bot"}`))
		}))
		DeferCleanup(gitlab.Close)
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(conn), conn)).To(Succeed())
		conn.Spec.URL = gitlab.URL
		Expect(k8sClient.Update(ctx, conn)).To(Succeed())

		token := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "scm-token", Namespace: ns},
			Data:       map[string][]byte{"token": []byte("good")},
		}
		Expect(k8sClient.Create(ctx, token)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, token) })

		r := &ScmConnectionReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(conn)}
		ready := func() *metav1.Condition {
			Expect(k8sClient.Get(ctx, req.NamespacedName, conn)).To(Succeed())
			return meta.FindStatusCondition(conn.Status.Conditions, "Ready")
		}

		res, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(DefaultSCMRecheckInterval), "5m unless the manager is told otherwise")
		Expect(ready().Status).To(Equal(metav1.ConditionTrue))

		By("rotating the token: nothing watches the Secret, so nothing happens yet")
		token.Data["token"] = []byte("rotated")
		Expect(k8sClient.Update(ctx, token)).To(Succeed())
		Expect(ready().Status).To(Equal(metav1.ConditionTrue))

		By("the requeue at the configured interval verifies the new token")
		r.RecheckInterval = 90 * time.Second
		res, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(scmRetryInterval), "a failing connection is retried sooner")
		Expect(ready().Reason).To(Equal("Unauthorized"))

		By("fixing the Secret: the retry makes the connection Ready again, due after the configured interval")
		token.Data["token"] = []byte("good")
		Expect(k8sClient.Update(ctx, token)).To(Succeed())
		res, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(90 * time.Second))
		Expect(ready().Status).To(Equal(metav1.ConditionTrue))
	})
})

var _ = Describe("The token Secret of a proposal's ScmConnection that the operator cannot read", func() {
	It("holds the proposal as an authentication problem that names the namespace and the chart value", func() {
		conn := &backflowv1alpha1.ScmConnection{
			ObjectMeta: metav1.ObjectMeta{Name: "held-conn", Namespace: "default"},
			Spec: backflowv1alpha1.ScmConnectionSpec{
				Provider: backflowv1alpha1.ScmProviderGitLab, URL: "https://gitlab.example.com",
				TokenSecretRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "tok"}, Key: "token"},
			},
		}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, conn) })

		_, err := scmAccessByName(ctx, NewSecretClient(k8sClient, forbiddingReader{}, 0), "default", "held-conn")
		Expect(err).To(MatchError(gitrepo.ErrAuth))
		Expect(err.Error()).To(And(ContainSubstring(`namespace "default"`), ContainSubstring("secretNamespaces")))
	})
})

// recordingCache records the kinds the manager's cache serves or watches.
type recordingCache struct {
	cache.Cache
	mu    sync.Mutex
	kinds []schema.GroupVersionKind
}

func (r *recordingCache) record(gvk schema.GroupVersionKind) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kinds = append(r.kinds, gvk)
}

func (r *recordingCache) seen() []schema.GroupVersionKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]schema.GroupVersionKind(nil), r.kinds...)
}

func (r *recordingCache) GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error) {
	if gvk, err := apiutil.GVKForObject(obj, scheme.Scheme); err == nil {
		r.record(gvk)
	}
	return r.Cache.GetInformer(ctx, obj, opts...)
}

func (r *recordingCache) GetInformerForKind(ctx context.Context, gvk schema.GroupVersionKind, opts ...cache.InformerGetOption) (cache.Informer, error) {
	r.record(gvk)
	return r.Cache.GetInformerForKind(ctx, gvk, opts...)
}

func (r *recordingCache) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if gvk, err := apiutil.GVKForObject(obj, scheme.Scheme); err == nil {
		r.record(gvk)
	}
	return r.Cache.Get(ctx, key, obj, opts...)
}

func (r *recordingCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if gvk, err := apiutil.GVKForObject(list, scheme.Scheme); err == nil {
		r.record(gvk)
	}
	return r.Cache.List(ctx, list, opts...)
}

var _ = Describe("The manager's cache", func() {
	It("never starts an informer for Secrets", func() {
		rec := &recordingCache{}
		mgr, err := manager.New(cfg, manager.Options{
			Scheme:  scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
			Client:  ClientOptions(),
			NewCache: func(c *rest.Config, o cache.Options) (cache.Cache, error) {
				inner, err := cache.New(c, o)
				rec.Cache = inner
				return rec, err
			},
			Controller: config.Controller{SkipNameValidation: new(true)},
		})
		Expect(err).NotTo(HaveOccurred())

		secrets := NewSecretClient(mgr.GetClient(), mgr.GetAPIReader(), time.Minute)
		Expect((&ScmConnectionReconciler{Client: secrets, Scheme: mgr.GetScheme()}).SetupWithManager(mgr)).To(Succeed())
		Expect((&DriftProposalReconciler{Client: secrets, Scheme: mgr.GetScheme()}).SetupWithManager(mgr)).To(Succeed())

		mgrCtx, stop := context.WithCancel(ctx)
		defer stop()
		go func() { defer GinkgoRecover(); Expect(mgr.Start(mgrCtx)).To(Succeed()) }()
		Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())

		By("reading a Secret through the operator's client, and through the manager's own")
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "no-informer", Namespace: "default"},
			Data:       map[string][]byte{"token": []byte("t")},
		})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "no-informer", Namespace: "default"}})
		})
		var got corev1.Secret
		Expect(secrets.Get(ctx, types.NamespacedName{Namespace: "default", Name: "no-informer"}, &got)).To(Succeed())
		Expect(mgr.GetClient().Get(ctx, types.NamespacedName{Namespace: "default", Name: "no-informer"}, &got)).To(Succeed())

		By("checking what the cache was asked for")
		seen := rec.seen()
		Expect(seen).NotTo(BeEmpty(), "the controllers watch their own kinds")
		for _, gvk := range seen {
			Expect(gvk.Kind).NotTo(Equal("Secret"), "an informer for Secrets was requested")
		}
		Expect(seen).To(ContainElement(HaveField("Kind", "ScmConnection")))
	})
})

var _ = Describe("An operator limited to get on Secrets in one namespace", func() {
	It("reads a Secret there, is Forbidden elsewhere and cannot list", func() {
		const allowed, denied = "secrets-allowed", "secrets-denied"
		for _, name := range []string{allowed, denied} {
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "token", Namespace: name}, Data: map[string][]byte{"token": []byte("t")},
			})).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "secrets-get", Namespace: allowed},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "secrets-get", Namespace: allowed},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "secrets-get"},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: "limited-operator"}},
		})).To(Succeed())

		user, err := testEnv.AddUser(envtest.User{Name: "limited-operator"}, nil)
		Expect(err).NotTo(HaveOccurred())
		limited, err := client.New(user.Config(), client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		c := NewSecretClient(k8sClient, limited, 0)

		var got corev1.Secret
		Eventually(func() error {
			return c.Get(ctx, types.NamespacedName{Namespace: allowed, Name: "token"}, &got)
		}, "20s", "200ms").Should(Succeed())

		err = c.Get(ctx, types.NamespacedName{Namespace: denied, Name: "token"}, &got)
		Expect(apierrors.IsForbidden(err)).To(BeTrue())
		Expect(isSecretForbidden(secretReadError(err, denied, "token"))).To(BeTrue())

		Expect(c.List(ctx, &corev1.SecretList{}, client.InNamespace(allowed))).To(MatchError(errListSecrets))
		// Even straight through the limited API client the server refuses a list.
		Expect(apierrors.IsForbidden(limited.List(ctx, &corev1.SecretList{}, client.InNamespace(allowed)))).To(BeTrue())
	})
})

// The operator holds no permission to list or watch Secrets, so no code may
// ask for it. This catches a Secret watch or list added to the controllers.
var _ = Describe("The controllers' source", func() {
	It("does not watch, own or list Secrets", func() {
		files, err := filepath.Glob("*.go")
		Expect(err).NotTo(HaveOccurred())
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			Expect(err).NotTo(HaveOccurred())
			for _, banned := range []string{"Watches(&corev1.Secret", "Owns(&corev1.Secret", "corev1.SecretList", "resources=secrets,verbs=get;list", "resources=secrets,verbs=get;watch"} {
				if f == "secrets.go" && banned == "corev1.SecretList" {
					continue // the guard against listing
				}
				Expect(string(src)).NotTo(ContainSubstring(banned), "%s", f)
			}
		}
	})
})
