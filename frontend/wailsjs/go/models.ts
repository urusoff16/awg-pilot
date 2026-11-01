export namespace flows {
	
	export class Row {
	    app: string;
	    host: string;
	    port: number;
	    proto: string;
	    tx: number;
	    rx: number;
	    conns: number;
	    last: number;
	
	    static createFrom(source: any = {}) {
	        return new Row(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.app = source["app"];
	        this.host = source["host"];
	        this.port = source["port"];
	        this.proto = source["proto"];
	        this.tx = source["tx"];
	        this.rx = source["rx"];
	        this.conns = source["conns"];
	        this.last = source["last"];
	    }
	}
	export class Report {
	    enabled: boolean;
	    day: string;
	    days: string[];
	    rows: Row[];
	
	    static createFrom(source: any = {}) {
	        return new Report(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.enabled = source["enabled"];
	        this.day = source["day"];
	        this.days = source["days"];
	        this.rows = this.convertValues(source["rows"], Row);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace keyapi {
	
	export class Country {
	    country_label: string;
	    iso_country_code: string;
	
	    static createFrom(source: any = {}) {
	        return new Country(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.country_label = source["country_label"];
	        this.iso_country_code = source["iso_country_code"];
	    }
	}
	export class User {
	    email: string;
	
	    static createFrom(source: any = {}) {
	        return new User(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.email = source["email"];
	    }
	}
	export class Request {
	    public_request_id: string;
	    simplified_status: string;
	    paid_until: string;
	    payment_link: string;
	    alias?: string;
	    is_brand_new: boolean;
	    number_of_available_keys: number;
	    key_limit: number;
	    user: User;
	    auto_renewal?: boolean;
	    price_rub?: number;
	    period?: number;
	    period_name?: string;
	
	    static createFrom(source: any = {}) {
	        return new Request(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.public_request_id = source["public_request_id"];
	        this.simplified_status = source["simplified_status"];
	        this.paid_until = source["paid_until"];
	        this.payment_link = source["payment_link"];
	        this.alias = source["alias"];
	        this.is_brand_new = source["is_brand_new"];
	        this.number_of_available_keys = source["number_of_available_keys"];
	        this.key_limit = source["key_limit"];
	        this.user = this.convertValues(source["user"], User);
	        this.auto_renewal = source["auto_renewal"];
	        this.price_rub = source["price_rub"];
	        this.period = source["period"];
	        this.period_name = source["period_name"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace main {
	
	export class Alert {
	    kind: string;
	    message: string;
	    action: string;
	
	    static createFrom(source: any = {}) {
	        return new Alert(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.kind = source["kind"];
	        this.message = source["message"];
	        this.action = source["action"];
	    }
	}
	export class ConfigInfo {
	    endpoint: string;
	    address: string[];
	    dns: string[];
	    mtu: number;
	    awg: string;
	
	    static createFrom(source: any = {}) {
	        return new ConfigInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.endpoint = source["endpoint"];
	        this.address = source["address"];
	        this.dns = source["dns"];
	        this.mtu = source["mtu"];
	        this.awg = source["awg"];
	    }
	}
	export class ProfileInfo {
	    id: string;
	    name: string;
	    has_key: boolean;
	    masked_key: string;
	    endpoint: string;
	    has_config: boolean;
	    config_for: string;
	
	    static createFrom(source: any = {}) {
	        return new ProfileInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.has_key = source["has_key"];
	        this.masked_key = source["masked_key"];
	        this.endpoint = source["endpoint"];
	        this.has_config = source["has_config"];
	        this.config_for = source["config_for"];
	    }
	}
	export class SpeedInfo {
	    rx_bps: number;
	    tx_bps: number;
	    ping_ms: number;
	    history: netstat.Point[];
	
	    static createFrom(source: any = {}) {
	        return new SpeedInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.rx_bps = source["rx_bps"];
	        this.tx_bps = source["tx_bps"];
	        this.ping_ms = source["ping_ms"];
	        this.history = this.convertValues(source["history"], netstat.Point);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class State {
	    has_key: boolean;
	    masked_key: string;
	    api_endpoint: string;
	    has_config: boolean;
	    config_for: string;
	    config_info?: ConfigInfo;
	    profiles: ProfileInfo[];
	    active_id: string;
	    settings: store.Settings;
	    device_source: string;
	    account?: keyapi.Request;
	    countries: keyapi.Country[];
	    connections: remotecfg.Connection[];
	    tunnel: tunnel.Stats;
	    warnings: string[];
	    service_ok: boolean;
	    autostart: boolean;
	    version: string;
	    speed: SpeedInfo;
	    alert?: Alert;
	    usage: usage.Summary;
	    russia_list: string;
	    speedtest: speedtest.State;
	    lang: string;
	    other_vpn: string;
	
	    static createFrom(source: any = {}) {
	        return new State(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.has_key = source["has_key"];
	        this.masked_key = source["masked_key"];
	        this.api_endpoint = source["api_endpoint"];
	        this.has_config = source["has_config"];
	        this.config_for = source["config_for"];
	        this.config_info = this.convertValues(source["config_info"], ConfigInfo);
	        this.profiles = this.convertValues(source["profiles"], ProfileInfo);
	        this.active_id = source["active_id"];
	        this.settings = this.convertValues(source["settings"], store.Settings);
	        this.device_source = source["device_source"];
	        this.account = this.convertValues(source["account"], keyapi.Request);
	        this.countries = this.convertValues(source["countries"], keyapi.Country);
	        this.connections = this.convertValues(source["connections"], remotecfg.Connection);
	        this.tunnel = this.convertValues(source["tunnel"], tunnel.Stats);
	        this.warnings = source["warnings"];
	        this.service_ok = source["service_ok"];
	        this.autostart = source["autostart"];
	        this.version = source["version"];
	        this.speed = this.convertValues(source["speed"], SpeedInfo);
	        this.alert = this.convertValues(source["alert"], Alert);
	        this.usage = this.convertValues(source["usage"], usage.Summary);
	        this.russia_list = source["russia_list"];
	        this.speedtest = this.convertValues(source["speedtest"], speedtest.State);
	        this.lang = source["lang"];
	        this.other_vpn = source["other_vpn"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class WidgetStatus {
	    state: string;
	    title: string;
	    detail: string;
	    rx: number;
	    tx: number;
	    ping: number;
	    lang: string;
	
	    static createFrom(source: any = {}) {
	        return new WidgetStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.state = source["state"];
	        this.title = source["title"];
	        this.detail = source["detail"];
	        this.rx = source["rx"];
	        this.tx = source["tx"];
	        this.ping = source["ping"];
	        this.lang = source["lang"];
	    }
	}

}

export namespace netstat {
	
	export class Point {
	    t: number;
	    rx: number;
	    tx: number;
	
	    static createFrom(source: any = {}) {
	        return new Point(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.t = source["t"];
	        this.rx = source["rx"];
	        this.tx = source["tx"];
	    }
	}

}

export namespace remotecfg {
	
	export class Connection {
	    title: string;
	    localized_title: Record<string, string>;
	    relative_path: string;
	    key_type: string;
	
	    static createFrom(source: any = {}) {
	        return new Connection(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.title = source["title"];
	        this.localized_title = source["localized_title"];
	        this.relative_path = source["relative_path"];
	        this.key_type = source["key_type"];
	    }
	}

}

export namespace routing {
	
	export class Settings {
	    mode: string;
	    entries: string[];
	    bypass_lan: boolean;
	    bypass_russia: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Settings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.mode = source["mode"];
	        this.entries = source["entries"];
	        this.bypass_lan = source["bypass_lan"];
	        this.bypass_russia = source["bypass_russia"];
	    }
	}

}

export namespace speedtest {
	
	export class State {
	    running: boolean;
	    phase: string;
	    mbps: number;
	    down_mbps: number;
	    up_mbps: number;
	    ip: string;
	    loc: string;
	    error: string;
	    // Go type: time
	    at: any;
	
	    static createFrom(source: any = {}) {
	        return new State(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.running = source["running"];
	        this.phase = source["phase"];
	        this.mbps = source["mbps"];
	        this.down_mbps = source["down_mbps"];
	        this.up_mbps = source["up_mbps"];
	        this.ip = source["ip"];
	        this.loc = source["loc"];
	        this.error = source["error"];
	        this.at = this.convertValues(source["at"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace store {
	
	export class Settings {
	    country: string;
	    key_type: string;
	    kill_switch: boolean;
	    dns_preset: string;
	    custom_dns: string[];
	    mtu: number;
	    auto_connect: boolean;
	    reconnect: boolean;
	    split: routing.Settings;
	    block_ipv6: boolean;
	    vpn_only_apps: string[];
	    close_to_tray: boolean;
	    notifications: boolean;
	    check_subscription: boolean;
	    auto_change_server: boolean;
	    lockdown_on_drop: boolean;
	    show_widget: boolean;
	    skin: string;
	    language: string;
	    app_mode: string;
	    app_packages: string[];
	    device_id: string;
	    user_agent: string;
	
	    static createFrom(source: any = {}) {
	        return new Settings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.country = source["country"];
	        this.key_type = source["key_type"];
	        this.kill_switch = source["kill_switch"];
	        this.dns_preset = source["dns_preset"];
	        this.custom_dns = source["custom_dns"];
	        this.mtu = source["mtu"];
	        this.auto_connect = source["auto_connect"];
	        this.reconnect = source["reconnect"];
	        this.split = this.convertValues(source["split"], routing.Settings);
	        this.block_ipv6 = source["block_ipv6"];
	        this.vpn_only_apps = source["vpn_only_apps"];
	        this.close_to_tray = source["close_to_tray"];
	        this.notifications = source["notifications"];
	        this.check_subscription = source["check_subscription"];
	        this.auto_change_server = source["auto_change_server"];
	        this.lockdown_on_drop = source["lockdown_on_drop"];
	        this.show_widget = source["show_widget"];
	        this.skin = source["skin"];
	        this.language = source["language"];
	        this.app_mode = source["app_mode"];
	        this.app_packages = source["app_packages"];
	        this.device_id = source["device_id"];
	        this.user_agent = source["user_agent"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace tunnel {
	
	export class Stats {
	    up: boolean;
	    endpoint: string;
	    // Go type: time
	    last_handshake: any;
	    rx_bytes: number;
	    tx_bytes: number;
	    // Go type: time
	    since: any;
	    kill_switch: boolean;
	    ipv6_blocked: boolean;
	    net_changes: number;
	    no_network: boolean;
	    blocked: boolean;
	    lockdown: boolean;
	    ping_ms: number;
	    ping_fails: number;
	
	    static createFrom(source: any = {}) {
	        return new Stats(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.up = source["up"];
	        this.endpoint = source["endpoint"];
	        this.last_handshake = this.convertValues(source["last_handshake"], null);
	        this.rx_bytes = source["rx_bytes"];
	        this.tx_bytes = source["tx_bytes"];
	        this.since = this.convertValues(source["since"], null);
	        this.kill_switch = source["kill_switch"];
	        this.ipv6_blocked = source["ipv6_blocked"];
	        this.net_changes = source["net_changes"];
	        this.no_network = source["no_network"];
	        this.blocked = source["blocked"];
	        this.lockdown = source["lockdown"];
	        this.ping_ms = source["ping_ms"];
	        this.ping_fails = source["ping_fails"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

export namespace usage {
	
	export class Day {
	    rx: number;
	    tx: number;
	
	    static createFrom(source: any = {}) {
	        return new Day(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.rx = source["rx"];
	        this.tx = source["tx"];
	    }
	}
	export class DayPoint {
	    date: string;
	    rx: number;
	    tx: number;
	
	    static createFrom(source: any = {}) {
	        return new DayPoint(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.date = source["date"];
	        this.rx = source["rx"];
	        this.tx = source["tx"];
	    }
	}
	export class Summary {
	    today: Day;
	    week: Day;
	    month: Day;
	    all: Day;
	    since: string;
	    last30: DayPoint[];
	
	    static createFrom(source: any = {}) {
	        return new Summary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.today = this.convertValues(source["today"], Day);
	        this.week = this.convertValues(source["week"], Day);
	        this.month = this.convertValues(source["month"], Day);
	        this.all = this.convertValues(source["all"], Day);
	        this.since = source["since"];
	        this.last30 = this.convertValues(source["last30"], DayPoint);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

