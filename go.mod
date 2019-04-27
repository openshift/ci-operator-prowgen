module github.com/openshift/ci-operator-prowgen

replace github.com/golang/lint => golang.org/x/lint v0.0.0-20190301231843-5614ed5bae6f

require (
	cloud.google.com/go v0.37.2
	github.com/getlantern/deepcopy v0.0.0-20160317154340-7f45deb8130a
	github.com/ghodss/yaml v1.0.0
	github.com/mattn/go-zglob v0.0.1
	github.com/openshift/ci-operator v0.0.0-20190418213341-cb1c3450ce47
	github.com/shurcooL/githubv4 v0.0.0-20180925043049-51d7b505e2e9
	github.com/sirupsen/logrus v1.2.0
	golang.org/x/oauth2 v0.0.0-20190226205417-e64efc72b421
	golang.org/x/sync v0.0.0-20190227155943-e225da77a7e6
	google.golang.org/api v0.3.0
	k8s.io/api v0.0.0-20181128191700-6db15a15d2d3
	k8s.io/apimachinery v0.0.0-20181128191346-49ce2735e507
	k8s.io/client-go v9.0.0+incompatible
	k8s.io/test-infra v0.0.0-20190419141755-2128cd49ec49
	sigs.k8s.io/yaml v1.1.0
)
