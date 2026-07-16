export namespace main {
	
	export class Settings {
	    rtspUrl: string;
	    version: string;
	
	    static createFrom(source: any = {}) {
	        return new Settings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.rtspUrl = source["rtspUrl"];
	        this.version = source["version"];
	    }
	}

}

