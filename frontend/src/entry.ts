// The same page serves the main window, the small status window (`awgpilot.exe --widget`) and
// the Android app. Each Windows process binds a different Go object, which tells them apart;
// on Android the WebView exposes window.AWGAndroid instead.
import {installAndroidBridge} from './android';

if ((window as any).AWGAndroid) installAndroidBridge();
const go = (window as any).go?.main ?? {};
if (go.App) {
    import('./main');
} else {
    import('./widget').then(m => m.runWidget());
}
