package wfp

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// AppRules confines selected programs to the VPN tunnel with WFP block filters.
//
// While the tunnel is up (tunLUID != 0) the programs may only use the tunnel interface;
// while it is down they have no network at all, so a reconnect never leaks their traffic.
// Loopback is always allowed. Filters live in a dynamic WFP session and disappear when the
// session closes or this process exits, so a crash cannot leave programs blocked forever.
type AppRules struct {
	mu      sync.Mutex
	session uintptr
}

// Apply replaces the active rules. An empty apps list removes all rules.
// The new session is committed before the old one closes, so there is no unprotected gap.
func (r *AppRules) Apply(apps []string, tunLUID uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(apps) == 0 {
		r.closeLocked()
		return nil
	}
	for _, p := range apps {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("программа не найдена: %s", p)
		}
	}
	session, err := openDynamicSession()
	if err != nil {
		return fmt.Errorf("не удалось открыть сеанс фильтров Windows (WFP): %w", err)
	}
	err = runTransaction(session, func(session uintptr) error {
		provider, sublayer, err := registerAppObjects(session)
		if err != nil {
			return err
		}
		for _, p := range apps {
			if err := blockAppOutsideTunnel(session, provider, sublayer, p, tunLUID); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
		}
		return nil
	})
	if err != nil {
		fwpmEngineClose0(session)
		return fmt.Errorf("не удалось установить правила для программ: %w", err)
	}
	r.closeLocked()
	r.session = session
	return nil
}

// Close removes all rules.
func (r *AppRules) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeLocked()
}

func (r *AppRules) closeLocked() {
	if r.session != 0 {
		fwpmEngineClose0(r.session)
		r.session = 0
	}
}

func openDynamicSession() (uintptr, error) {
	dd, err := createWtFwpmDisplayData0("AWG Pilot", "AWG Pilot per-app rules")
	if err != nil {
		return 0, err
	}
	s := wtFwpmSession0{displayData: *dd, flags: cFWPM_SESSION_FLAG_DYNAMIC, txnWaitTimeoutInMSec: windows.INFINITE}
	h := uintptr(0)
	if err := fwpmEngineOpen0(nil, cRPC_C_AUTHN_WINNT, nil, &s, unsafe.Pointer(&h)); err != nil {
		return 0, err
	}
	return h, nil
}

func registerAppObjects(session uintptr) (provider, sublayer windows.GUID, err error) {
	if provider, err = windows.GenerateGUID(); err != nil {
		return
	}
	if sublayer, err = windows.GenerateGUID(); err != nil {
		return
	}
	dd, err := createWtFwpmDisplayData0("AWG Pilot", "AWG Pilot provider")
	if err != nil {
		return
	}
	if err = fwpmProviderAdd0(session, &wtFwpmProvider0{providerKey: provider, displayData: *dd}, 0); err != nil {
		return
	}
	dd, err = createWtFwpmDisplayData0("AWG Pilot per-app", "Programs allowed only through the VPN")
	if err != nil {
		return
	}
	err = fwpmSubLayerAdd0(session, &wtFwpmSublayer0{subLayerKey: sublayer, displayData: *dd, providerKey: &provider, weight: ^uint16(0)}, 0)
	return
}

func blockAppOutsideTunnel(session uintptr, provider, sublayer windows.GUID, path string, tunLUID uint64) error {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	var appID *wtFwpByteBlob
	if err := fwpmGetAppIdFromFileName0(pathPtr, unsafe.Pointer(&appID)); err != nil {
		return err
	}
	defer fwpmFreeMemory0(unsafe.Pointer(&appID))
	if appID == nil {
		return errors.New("Windows не вернула идентификатор программы")
	}

	conditions := []wtFwpmFilterCondition0{
		{
			fieldKey:       cFWPM_CONDITION_ALE_APP_ID,
			matchType:      cFWP_MATCH_EQUAL,
			conditionValue: wtFwpConditionValue0{_type: cFWP_BYTE_BLOB_TYPE, value: uintptr(unsafe.Pointer(appID))},
		},
		{
			fieldKey:       cFWPM_CONDITION_FLAGS,
			matchType:      cFWP_MATCH_FLAGS_NONE_SET,
			conditionValue: wtFwpConditionValue0{_type: cFWP_UINT32, value: uintptr(cFWP_CONDITION_FLAG_IS_LOOPBACK)},
		},
	}
	if tunLUID != 0 {
		conditions = append(conditions, wtFwpmFilterCondition0{
			fieldKey:       cFWPM_CONDITION_IP_LOCAL_INTERFACE,
			matchType:      cFWP_MATCH_NOT_EQUAL,
			conditionValue: wtFwpConditionValue0{_type: cFWP_UINT64, value: uintptr(unsafe.Pointer(&tunLUID))},
		})
	}
	filter := wtFwpmFilter0{
		providerKey:         &provider,
		subLayerKey:         sublayer,
		weight:              filterWeight(15),
		numFilterConditions: uint32(len(conditions)),
		filterCondition:     &conditions[0],
		action:              wtFwpmAction0{_type: cFWP_ACTION_BLOCK},
	}
	layers := []struct {
		key  windows.GUID
		name string
	}{
		{cFWPM_LAYER_ALE_AUTH_CONNECT_V4, "Block app outbound IPv4 outside VPN"},
		{cFWPM_LAYER_ALE_AUTH_RECV_ACCEPT_V4, "Block app inbound IPv4 outside VPN"},
		{cFWPM_LAYER_ALE_AUTH_CONNECT_V6, "Block app outbound IPv6 outside VPN"},
		{cFWPM_LAYER_ALE_AUTH_RECV_ACCEPT_V6, "Block app inbound IPv6 outside VPN"},
	}
	for _, l := range layers {
		dd, err := createWtFwpmDisplayData0(l.name, path)
		if err != nil {
			return err
		}
		filter.displayData = *dd
		filter.layerKey = l.key
		id := uint64(0)
		if err := fwpmFilterAdd0(session, &filter, 0, &id); err != nil {
			return err
		}
	}
	runtime.KeepAlive(&tunLUID) // referenced only through a uintptr inside the condition
	return nil
}
