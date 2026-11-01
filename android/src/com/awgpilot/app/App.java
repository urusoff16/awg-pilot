package com.awgpilot.app;

import android.app.Application;

public final class App extends Application {
    @Override
    public void onCreate() {
        super.onCreate();
        Notifier.createChannels(this);
        Core.init(this);
    }
}
