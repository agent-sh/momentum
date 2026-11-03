package opencode

// These read-only projections put both database generations into the scanner's
// message/part shape. A migrated session uses V2, so its legacy copy is not read.
func (src *Source) sessionRows() string {
	if !src.v2 {
		return "session"
	}
	v2 := `SELECT id,NULLIF(parent_id,'') AS parent_id,directory,title,time_created,time_updated,time_archived FROM session_v2`
	if src.legacy {
		v2 += ` UNION ALL SELECT id,parent_id,directory,title,time_created,time_updated,time_archived FROM session
WHERE id NOT IN (SELECT id FROM session_v2)`
	}
	return "(" + v2 + ")"
}

func (src *Source) messageRows() string {
	if !src.v2 {
		return "message"
	}
	v2 := `SELECT id,session_id,time_created,
json_set(data,'$.role',type,'$.modelID',COALESCE(json_extract(data,'$.model.id'),''),
'$.error',CASE WHEN json_type(data,'$.error')='object' THEN json_object(
'name',COALESCE(json_extract(data,'$.error.type'),json_extract(data,'$.error.name'),'Error'),
'data',json_object('message',COALESCE(json_extract(data,'$.error.message'),''))) ELSE NULL END) AS data
FROM session_message`
	if src.legacy {
		v2 += ` UNION ALL SELECT id,session_id,time_created,data FROM message WHERE session_id NOT IN (SELECT id FROM session_v2)`
	}
	return "(" + v2 + ")"
}

func (src *Source) partRows() string {
	if !src.v2 {
		return "part"
	}
	v2 := `SELECT m.id||':'||j.key AS id,m.id AS message_id,m.session_id,m.time_created,
json_set(j.value,'$.tool',COALESCE(json_extract(j.value,'$.name'),''),
'$.state.time.start',COALESCE(json_extract(j.value,'$.time.created'),m.time_created)) AS data
FROM session_message m JOIN json_each(m.data,'$.content') j WHERE m.type='assistant'
UNION ALL SELECT id||':text',id,session_id,time_created,json_object('type','text','text',json_extract(data,'$.text'))
FROM session_message WHERE type='user'`
	if src.legacy {
		v2 += ` UNION ALL SELECT id,message_id,session_id,time_created,data FROM part WHERE session_id NOT IN (SELECT id FROM session_v2)`
	}
	return "(" + v2 + ")"
}
