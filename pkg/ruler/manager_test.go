// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/ruler/manager_test.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package ruler

import (
	"context"
	"io"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/test"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/rulefmt"
	"github.com/prometheus/prometheus/notifier"
	"github.com/prometheus/prometheus/rules"
	promRules "github.com/prometheus/prometheus/rules"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
	"gopkg.in/yaml.v3"

	"github.com/grafana/mimir/pkg/ruler/rulespb"
	testutil "github.com/grafana/mimir/pkg/util/test"
)

func TestDefaultMultiTenantManager_SyncFullRuleGroups(t *testing.T) {
	const (
		user1 = "user-1"
		user2 = "user-2"
	)

	var (
		ctx         = context.Background()
		logger      = testutil.NewTestingLogger(t)
		user1Group1 = createRuleGroup("group-1", user1, createRecordingRule("count:metric_1", "count(metric_1)"))
		user2Group1 = createRuleGroup("group-1", user2, createRecordingRule("sum:metric_1", "sum(metric_1)"))
	)

	m, err := NewDefaultMultiTenantManager(Config{RulePath: t.TempDir()}, managerMockFactory, nil, logger, nil)
	require.NoError(t, err)

	// Initialise the manager with some rules and start it.
	m.SyncFullRuleGroups(ctx, map[string]rulespb.RuleGroupList{
		user1: {user1Group1},
		user2: {user2Group1},
	})
	m.Start()

	initialUser1Manager := assertManagerMockRunningForUser(t, m, user1)
	initialUser2Manager := assertManagerMockRunningForUser(t, m, user2)

	assertRuleGroupsMappedOnDisk(t, m, user1, rulespb.RuleGroupList{user1Group1})
	assertRuleGroupsMappedOnDisk(t, m, user2, rulespb.RuleGroupList{user2Group1})

	t.Run("calling SyncFullRuleGroups() with an empty map stops all managers", func(t *testing.T) {
		m.SyncFullRuleGroups(ctx, nil)

		// Ensure the ruler manager has been stopped for all users.
		assertManagerMockStopped(t, initialUser1Manager)
		assertManagerMockStopped(t, initialUser2Manager)
		assertManagerMockNotRunningForUser(t, m, user1)
		assertManagerMockNotRunningForUser(t, m, user2)

		// Ensure the files have been removed from disk.
		assertRuleGroupsMappedOnDisk(t, m, user1, nil)
		assertRuleGroupsMappedOnDisk(t, m, user2, nil)

		// Check metrics.
		assert.Equal(t, 0.0, promtest.ToFloat64(m.managersTotal))
	})

	t.Run("calling SyncFullRuleGroups() with the previous config restores the managers", func(t *testing.T) {
		m.SyncFullRuleGroups(ctx, map[string]rulespb.RuleGroupList{
			user1: {user1Group1},
			user2: {user2Group1},
		})

		// Ensure the ruler manager has been started.
		currUser1Manager := assertManagerMockRunningForUser(t, m, user1)
		currUser2Manager := assertManagerMockRunningForUser(t, m, user2)
		assert.NotEqual(t, currUser1Manager, initialUser1Manager)
		assert.NotEqual(t, currUser2Manager, initialUser2Manager)

		// Ensure the files have been mapped to disk.
		assertRuleGroupsMappedOnDisk(t, m, user1, rulespb.RuleGroupList{user1Group1})
		assertRuleGroupsMappedOnDisk(t, m, user2, rulespb.RuleGroupList{user2Group1})

		// Check metrics.
		assert.Equal(t, 2.0, promtest.ToFloat64(m.managersTotal))
	})

	t.Run("calling Stop() should stop all managers", func(t *testing.T) {
		// Pre-condition check.
		currUser1Manager := assertManagerMockRunningForUser(t, m, user1)
		currUser2Manager := assertManagerMockRunningForUser(t, m, user2)

		m.Stop()

		assertManagerMockStopped(t, currUser1Manager)
		assertManagerMockStopped(t, currUser2Manager)

		assertManagerMockNotRunningForUser(t, m, user1)
		assertManagerMockNotRunningForUser(t, m, user2)

		// Ensure the files have been removed from disk.
		assertRuleGroupsMappedOnDisk(t, m, user1, nil)
		assertRuleGroupsMappedOnDisk(t, m, user2, nil)
	})
}

func TestDefaultMultiTenantManager_SyncPartialRuleGroups(t *testing.T) {
	const (
		user1 = "user-1"
		user2 = "user-2"
	)

	var (
		ctx         = context.Background()
		logger      = testutil.NewTestingLogger(t)
		user1Group1 = createRuleGroup("group-1", user1, createRecordingRule("count:metric_1", "count(metric_1)"))
		user1Group2 = createRuleGroup("group-2", user1, createRecordingRule("count:metric_2", "count(metric_2)"))
		user2Group1 = createRuleGroup("group-1", user2, createRecordingRule("sum:metric_1", "sum(metric_1)"))
	)

	m, err := NewDefaultMultiTenantManager(Config{RulePath: t.TempDir()}, managerMockFactory, nil, logger, nil)
	require.NoError(t, err)
	t.Cleanup(m.Stop)

	// Initialise the manager with some rules and start it.
	m.SyncFullRuleGroups(ctx, map[string]rulespb.RuleGroupList{
		user1: {user1Group1},
		user2: {user2Group1},
	})
	m.Start()

	initialUser1Manager := assertManagerMockRunningForUser(t, m, user1)
	initialUser2Manager := assertManagerMockRunningForUser(t, m, user2)

	// Ensure the right rule groups have been mapped on disk.
	assertRuleGroupsMappedOnDisk(t, m, user1, rulespb.RuleGroupList{user1Group1})
	assertRuleGroupsMappedOnDisk(t, m, user2, rulespb.RuleGroupList{user2Group1})

	t.Run("calling SyncPartialRuleGroups() with an empty map should be a no-op", func(t *testing.T) {
		m.SyncPartialRuleGroups(ctx, nil)

		// Ensure the per-tenant manager has not changed.
		currUser1Manager := assertManagerMockRunningForUser(t, m, user1)
		currUser2Manager := assertManagerMockRunningForUser(t, m, user2)
		assert.Equal(t, initialUser1Manager, currUser1Manager)
		assert.Equal(t, initialUser2Manager, currUser2Manager)

		// Ensure the right rule groups have been mapped on disk.
		assertRuleGroupsMappedOnDisk(t, m, user1, rulespb.RuleGroupList{user1Group1})
		assertRuleGroupsMappedOnDisk(t, m, user2, rulespb.RuleGroupList{user2Group1})

		// Check metrics.
		assert.Equal(t, 2.0, promtest.ToFloat64(m.managersTotal))
	})

	t.Run("calling SyncPartialRuleGroups() with a subset of users should re-sync them", func(t *testing.T) {
		m.SyncPartialRuleGroups(ctx, map[string]rulespb.RuleGroupList{
			user1: {user1Group1, user1Group2},
		})

		// Ensure the per-tenant manager has not changed.
		currUser1Manager := assertManagerMockRunningForUser(t, m, user1)
		currUser2Manager := assertManagerMockRunningForUser(t, m, user2)
		assert.Equal(t, initialUser1Manager, currUser1Manager)
		assert.Equal(t, initialUser2Manager, currUser2Manager)

		// Ensure the right rule groups have been mapped on disk.
		assertRuleGroupsMappedOnDisk(t, m, user1, rulespb.RuleGroupList{user1Group1, user1Group2})
		assertRuleGroupsMappedOnDisk(t, m, user2, rulespb.RuleGroupList{user2Group1})

		// Check metrics.
		assert.Equal(t, 2.0, promtest.ToFloat64(m.managersTotal))
	})

	t.Run("calling SyncPartialRuleGroups() with a user with no rule groups should stop its manager", func(t *testing.T) {
		m.SyncPartialRuleGroups(ctx, map[string]rulespb.RuleGroupList{
			user1: nil,
		})

		// Ensure the ruler manager has been stopped for the user with no rule groups.
		assertManagerMockStopped(t, initialUser1Manager)
		assertManagerMockNotRunningForUser(t, m, user1)

		// Ensure the ruler manager is still running for other users.
		currUser2Manager := assertManagerMockRunningForUser(t, m, user2)
		assert.Equal(t, initialUser2Manager, currUser2Manager)

		// Ensure the right rule groups have been mapped on disk.
		assertRuleGroupsMappedOnDisk(t, m, user1, nil)
		assertRuleGroupsMappedOnDisk(t, m, user2, rulespb.RuleGroupList{user2Group1})

		// Check metrics.
		assert.Equal(t, 1.0, promtest.ToFloat64(m.managersTotal))
	})
}

func TestFilterRuleGroupsByNotEmptyUsers(t *testing.T) {
	tests := map[string]struct {
		configs         map[string]rulespb.RuleGroupList
		expectedConfigs map[string]rulespb.RuleGroupList
		expectedRemoved map[string]struct{}
	}{
		"should return an empty map on empty input": {
			configs:         nil,
			expectedConfigs: nil,
		},
		"should return the input map on no empty users": {
			configs: map[string]rulespb.RuleGroupList{
				"user-1": {
					createRuleGroup("group-1", "user-1", createRecordingRule("record:1", "1"), createAlertingRule("alert-2", "2"), createRecordingRule("record:3", "3")),
				},
				"user-2": {
					createRuleGroup("group-1", "user-2", createRecordingRule("record:1", "1"), createAlertingRule("alert-2", "2"), createRecordingRule("record:3", "3")),
					createRuleGroup("group-2", "user-2", createRecordingRule("record:4", "4"), createRecordingRule("record:5", "5")),
				},
			},
			expectedConfigs: map[string]rulespb.RuleGroupList{
				"user-1": {
					createRuleGroup("group-1", "user-1", createRecordingRule("record:1", "1"), createAlertingRule("alert-2", "2"), createRecordingRule("record:3", "3")),
				},
				"user-2": {
					createRuleGroup("group-1", "user-2", createRecordingRule("record:1", "1"), createAlertingRule("alert-2", "2"), createRecordingRule("record:3", "3")),
					createRuleGroup("group-2", "user-2", createRecordingRule("record:4", "4"), createRecordingRule("record:5", "5")),
				},
			},
		},
		"should remove users with no rule groups from the input map": {
			configs: map[string]rulespb.RuleGroupList{
				"user-1": {},
				"user-2": {
					createRuleGroup("group-1", "user-2", createRecordingRule("record:1", "1"), createAlertingRule("alert-2", "2"), createRecordingRule("record:3", "3")),
					createRuleGroup("group-2", "user-2", createRecordingRule("record:4", "4"), createRecordingRule("record:5", "5")),
				},
				"user-3": {},
			},
			expectedConfigs: map[string]rulespb.RuleGroupList{
				"user-2": {
					createRuleGroup("group-1", "user-2", createRecordingRule("record:1", "1"), createAlertingRule("alert-2", "2"), createRecordingRule("record:3", "3")),
					createRuleGroup("group-2", "user-2", createRecordingRule("record:4", "4"), createRecordingRule("record:5", "5")),
				},
			},
			expectedRemoved: map[string]struct{}{
				"user-1": {},
				"user-3": {},
			},
		},
	}

	for testName, testData := range tests {
		t.Run(testName, func(t *testing.T) {
			actualConfigs, actualRemoved := filterRuleGroupsByNotEmptyUsers(testData.configs)
			assert.Equal(t, testData.expectedConfigs, actualConfigs)
			assert.Equal(t, testData.expectedRemoved, actualRemoved)
		})
	}
}

func getManager(m *DefaultMultiTenantManager, user string) RulesManager {
	m.userManagerMtx.RLock()
	defer m.userManagerMtx.RUnlock()

	return m.userManagers[user]
}

func assertManagerMockRunningForUser(t *testing.T, m *DefaultMultiTenantManager, userID string) *managerMock {
	t.Helper()

	rm := getManager(m, userID)
	require.NotNil(t, rm)

	// The ruler manager start is async, so we poll it.
	test.Poll(t, 1*time.Second, true, func() interface{} {
		return rm.(*managerMock).running.Load()
	})

	return rm.(*managerMock)
}

func assertManagerMockNotRunningForUser(t *testing.T, m *DefaultMultiTenantManager, userID string) {
	t.Helper()

	rm := getManager(m, userID)
	require.Nil(t, rm)
}

func assertManagerMockStopped(t *testing.T, m *managerMock) {
	t.Helper()

	// The ruler manager stop is async, so we poll it.
	test.Poll(t, 1*time.Second, false, func() interface{} {
		return m.running.Load()
	})
}

func assertRuleGroupsMappedOnDisk(t *testing.T, m *DefaultMultiTenantManager, userID string, expectedRuleGroups rulespb.RuleGroupList) {
	t.Helper()

	// Verify that the rule groups have been mapped on disk for the given user.
	users, err := m.mapper.users()
	require.NoError(t, err)

	if len(expectedRuleGroups) > 0 {
		require.Contains(t, users, userID)
	} else {
		require.NotContains(t, users, userID)
	}

	// Verify the content of the rule groups mapped to disk.
	for namespace, expectedFormattedRuleGroups := range expectedRuleGroups.Formatted() {
		// The mapper sort groups by name in reverse order, so we apply the same sorting
		// here to expected groups.
		sort.Slice(expectedFormattedRuleGroups, func(i, j int) bool {
			return expectedFormattedRuleGroups[i].Name > expectedFormattedRuleGroups[j].Name
		})

		expectedYAML, err := yaml.Marshal(rulefmt.RuleGroups{Groups: expectedFormattedRuleGroups})
		require.NoError(t, err)

		path := filepath.Join(m.mapper.Path, userID, namespace)
		file, err := m.mapper.FS.Open(path)
		require.NoError(t, err)

		content, err := io.ReadAll(file)
		require.NoError(t, err)
		assert.Equal(t, string(expectedYAML), string(content))

		require.NoError(t, file.Close())
	}
}

func managerMockFactory(_ context.Context, _ string, _ *notifier.Manager, _ log.Logger, _ prometheus.Registerer) RulesManager {
	return &managerMock{done: make(chan struct{})}
}

type managerMock struct {
	running atomic.Bool
	done    chan struct{}
}

func (m *managerMock) Run() {
	defer m.running.Store(false)
	m.running.Store(true)
	<-m.done
}

func (m *managerMock) Stop() {
	close(m.done)
}

func (m *managerMock) Update(interval time.Duration, files []string, externalLabels labels.Labels, externalURL string, groupEvalIterationFunc rules.GroupEvalIterationFunc) error {
	return nil
}

func (m *managerMock) RuleGroups() []*promRules.Group {
	return nil
}
