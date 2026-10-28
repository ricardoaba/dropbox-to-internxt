export namespace main {
	
	export class Config {
	    sourceDir: string;
	    remoteRoot: string;
	    statePath: string;
	    webdavUrl: string;
	    webdavUser: string;
	    webdavPass: string;
	    insecure: boolean;
	    batchFiles: number;
	    batchMB: number;
	    verify: string;
	    parallel: number;
	    retries: number;
	    quarantineAfter: number;
	    maxFileGB: number;
	    excludes: string[];
	    composeDir: string;
	    scheduleMinutes: number;
	    inxtUser: string;
	    inxtPassword: string;
	    inxtOtpToken: string;
	    fetchOnlineOnly: boolean;
	    dropboxAppKey: string;
	    dropboxRefreshToken: string;
	    dropboxAuthError?: string;
	    dropboxAccount: string;
	
	    static createFrom(source: any = {}) {
	        return new Config(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.sourceDir = source["sourceDir"];
	        this.remoteRoot = source["remoteRoot"];
	        this.statePath = source["statePath"];
	        this.webdavUrl = source["webdavUrl"];
	        this.webdavUser = source["webdavUser"];
	        this.webdavPass = source["webdavPass"];
	        this.insecure = source["insecure"];
	        this.batchFiles = source["batchFiles"];
	        this.batchMB = source["batchMB"];
	        this.verify = source["verify"];
	        this.parallel = source["parallel"];
	        this.retries = source["retries"];
	        this.quarantineAfter = source["quarantineAfter"];
	        this.maxFileGB = source["maxFileGB"];
	        this.excludes = source["excludes"];
	        this.composeDir = source["composeDir"];
	        this.scheduleMinutes = source["scheduleMinutes"];
	        this.inxtUser = source["inxtUser"];
	        this.inxtPassword = source["inxtPassword"];
	        this.inxtOtpToken = source["inxtOtpToken"];
	        this.fetchOnlineOnly = source["fetchOnlineOnly"];
	        this.dropboxAppKey = source["dropboxAppKey"];
	        this.dropboxRefreshToken = source["dropboxRefreshToken"];
	        this.dropboxAuthError = source["dropboxAuthError"];
	        this.dropboxAccount = source["dropboxAccount"];
	    }
	}
	export class ExcludeRow {
	    pattern: string;
	    target: string;
	    kind: string;
	    exists: boolean;
	    matches: number;
	    bytes: number;
	    samples: string[];
	    status: string;
	    error: string;
	    time: string;
	
	    static createFrom(source: any = {}) {
	        return new ExcludeRow(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.pattern = source["pattern"];
	        this.target = source["target"];
	        this.kind = source["kind"];
	        this.exists = source["exists"];
	        this.matches = source["matches"];
	        this.bytes = source["bytes"];
	        this.samples = source["samples"];
	        this.status = source["status"];
	        this.error = source["error"];
	        this.time = source["time"];
	    }
	}
	export class ExcludeReport {
	    rows: ExcludeRow[];
	    defaults: string[];
	    files: number;
	    excluded: number;
	    excludedBytes: number;
	
	    static createFrom(source: any = {}) {
	        return new ExcludeReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.rows = this.convertValues(source["rows"], ExcludeRow);
	        this.defaults = source["defaults"];
	        this.files = source["files"];
	        this.excluded = source["excluded"];
	        this.excludedBytes = source["excludedBytes"];
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
	
	export class FailedRow {
	    path: string;
	    size: number;
	    error: string;
	    time: string;
	
	    static createFrom(source: any = {}) {
	        return new FailedRow(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.size = source["size"];
	        this.error = source["error"];
	        this.time = source["time"];
	    }
	}
	export class FileRow {
	    path: string;
	    size: number;
	
	    static createFrom(source: any = {}) {
	        return new FileRow(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.size = source["size"];
	    }
	}
	export class Summary {
	    totalFiles: number;
	    totalBytes: number;
	    done: number;
	    doneBytes: number;
	    pending: number;
	    pendingBytes: number;
	    retries: number;
	    changed: number;
	    skipped: number;
	    skippedBytes: number;
	    tooLarge: FileRow[];
	    percent: number;
	    scannedAt: string;
	    placeholders: number;
	    placeholdersSkipped: number;
	    emptyUploads: number;
	    quarantined: FailedRow[];
	    quarantinedBytes: number;
	
	    static createFrom(source: any = {}) {
	        return new Summary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.totalFiles = source["totalFiles"];
	        this.totalBytes = source["totalBytes"];
	        this.done = source["done"];
	        this.doneBytes = source["doneBytes"];
	        this.pending = source["pending"];
	        this.pendingBytes = source["pendingBytes"];
	        this.retries = source["retries"];
	        this.changed = source["changed"];
	        this.skipped = source["skipped"];
	        this.skippedBytes = source["skippedBytes"];
	        this.tooLarge = this.convertValues(source["tooLarge"], FileRow);
	        this.percent = source["percent"];
	        this.scannedAt = source["scannedAt"];
	        this.placeholders = source["placeholders"];
	        this.placeholdersSkipped = source["placeholdersSkipped"];
	        this.emptyUploads = source["emptyUploads"];
	        this.quarantined = this.convertValues(source["quarantined"], FailedRow);
	        this.quarantinedBytes = source["quarantinedBytes"];
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

